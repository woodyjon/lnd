package itest

import (
	"bytes"
	"context"
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

func testSingleHopInvoice(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// Open a channel with 100k satoshis between Alice and Bob with Alice being
	// the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	chanPoint := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	chanPointTXID, err := lnrpc.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	chanOutPoint := wire.OutPoint{
		Hash:  *chanPointTXID,
		Index: chanPoint.OutputIndex,
	}

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("A"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp, err := net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for Alice to recognize and advertise the new channel generated
	// above.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	resp := sendAndAssertSuccess(
		t, net.Alice, &routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)
	if hex.EncodeToString(preimage) != resp.PaymentPreimage {
		t.Fatalf("preimage mismatch: expected %v, got %v", preimage,
			resp.PaymentPreimage)
	}

	// Bob's invoice should now be found and marked as settled.
	payHash := &lnrpc.PaymentHash{
		RHash: invoiceResp.RHash,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	dbInvoice, err := net.Bob.LookupInvoice(ctxt, payHash)
	if err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}
	if !dbInvoice.Settled { // nolint:staticcheck
		t.Fatalf("bob's invoice should be marked as settled: %v",
			spew.Sdump(dbInvoice))
	}

	// With the payment completed, check the amount paid.
	assertAmountPaid(t, "Alice => Bob", net.Bob, chanOutPoint, int64(0), paymentAmt)
	assertAmountPaid(t, "Alice => Bob", net.Alice, chanOutPoint, paymentAmt, int64(0))

	// add Carol at the end of the loop
	// Open a channel with 100k satoshis between Bob and Carol with Bob being
	// the sole funder of the channel.
	carolArgs := []string{}
	carol := net.NewNode(t.t, "Carol", carolArgs)
	defer shutdownAndAssert(net, t, carol)
	net.ConnectNodes(t.t, carol, net.Bob)
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)
	chanBCPoint := openChannelAndAssert(
		t, net, net.Bob, carol,
		lntest.OpenChannelParams{
		    Amt: chanAmt,
		},
	)
	chanBCPointTXID, err := lnrpc.GetChanPointFundingTxid(chanBCPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	chanBCOutPoint := wire.OutPoint{
		Hash:  *chanBCPointTXID,
		Index: chanBCPoint.OutputIndex,
	}

	// Now that the channel is open, create an invoice for Carol which
	// expects a payment of 1000 satoshis from Alice 
	invoiceCarol := &lnrpc.Invoice{
		Memo:      "testing payment from Alice to Carol",
		Value:     paymentAmt,
	}
	invoiceCarolResp, err := carol.AddInvoice(ctxb, invoiceCarol)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for the channels to be recognized and advertised
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanBCPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+"timeout: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(ctxt, chanBCPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+"timeout: %v", err)
	}

	const bobBaseFeeSat = 42
	const bobFeeRatePPM = 100000
	updateChannelPolicy(
		t, net.Bob, chanBCPoint, bobBaseFeeSat*1000,
		bobFeeRatePPM, chainreg.DefaultBitcoinTimeLockDelta, calculateMaxHtlc(chanAmt),
		carol,
	)

	// With the invoice for Carol added, send a payment towards Alice paying
	// to the above generated invoice.
	resp = sendAndAssertSuccess(
		t, net.Alice, &routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceCarolResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// Carol's invoice should now be found and marked as settled.
	payHash = &lnrpc.PaymentHash{
		RHash: invoiceCarolResp.RHash,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	dbInvoice, err = carol.LookupInvoice(ctxt, payHash)
	if err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}
	if !dbInvoice.Settled { // nolint:staticcheck
		t.Fatalf("Carol's invoice should be marked as settled: %v",
			spew.Sdump(dbInvoice))
	}

	// With the payment completed, check the amount paid.
	assertAmountPaid(t, "Bob => Carol", carol, chanBCOutPoint, int64(0), paymentAmt)
	assertAmountPaid(t, "Bob => Carol", net.Bob, chanBCOutPoint, paymentAmt, int64(0))
	// To forward a payment of 1000 sat, Bob is charging a fee of
	// 42 sat + 10% = 142 sat. (+ Alice already sent the paymentAmt of 1000 sats to Bob)
	expectedAmountAtoB := paymentAmt + paymentAmt * bobFeeRatePPM / 1000000 + bobBaseFeeSat
	assertAmountPaid(t, "Alice => Bob", net.Bob, chanOutPoint, int64(0), int64(paymentAmt + expectedAmountAtoB))
	assertAmountPaid(t, "Alice => Bob", net.Alice, chanOutPoint, int64(paymentAmt + expectedAmountAtoB), int64(0))

	closeChannelAndAssert(t, net, net.Bob, chanBCPoint, false)


	// Create another invoice for Bob, this time leaving off the preimage
	// to one will be randomly generated. We'll test the proper
	// encoding/decoding of the zpay32 payment requests.
	invoice = &lnrpc.Invoice{
		Memo:  "test3",
		Value: paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err = net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Next send another payment, but this time using a zpay32 encoded
	// invoice rather than manually specifying the payment details.
	sendAndAssertSuccess(
		t, net.Alice, &routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// The second payment should also have succeeded, with the balances
	// being update accordingly.
	err = wait.NoError(
		assertAmountSent(btcutil.Amount(2*paymentAmt + expectedAmountAtoB), net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Next send a keysend payment.
	keySendPreimage := lntypes.Preimage{3, 4, 5, 11}
	keySendHash := keySendPreimage.Hash()

	sendAndAssertSuccess(
		t, net.Alice, &routerrpc.SendPaymentRequest{
			Dest:           net.Bob.PubKey[:],
			Amt:            paymentAmt,
			FinalCltvDelta: 40,
			PaymentHash:    keySendHash[:],
			DestCustomRecords: map[uint64][]byte{
				record.KeySendType: keySendPreimage[:],
			},
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// The keysend payment should also have succeeded, with the balances
	// being update accordingly.
	err = wait.NoError(
		assertAmountSent(btcutil.Amount(3*paymentAmt + expectedAmountAtoB), net.Alice, net.Bob),
		3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Assert that the invoice has the proper AMP fields set, since the
	// legacy keysend payment should have been promoted into an AMP payment
	// internally.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	keysendInvoice, err := net.Bob.LookupInvoice(
		ctxt, &lnrpc.PaymentHash{
			RHash: keySendHash[:],
		},
	)
	require.NoError(t.t, err)
	require.Equal(t.t, 1, len(keysendInvoice.Htlcs))
	htlc := keysendInvoice.Htlcs[0]
	require.Equal(t.t, uint64(0), htlc.MppTotalAmtMsat)
	require.Nil(t.t, htlc.Amp)

	// Now create an invoice and specify routing hints.
	// We will test that the routing hints are encoded properly.
	hintChannel := lnwire.ShortChannelID{BlockHeight: 10}
	bobPubKey := hex.EncodeToString(net.Bob.PubKey[:])
	hints := []*lnrpc.RouteHint{
		{
			HopHints: []*lnrpc.HopHint{
				{
					NodeId:                    bobPubKey,
					ChanId:                    hintChannel.ToUint64(),
					FeeBaseMsat:               1,
					FeeProportionalMillionths: 1000000,
					CltvExpiryDelta:           20,
				},
			},
		},
	}

	invoice = &lnrpc.Invoice{
		Memo:       "hints",
		Value:      paymentAmt,
		RouteHints: hints,
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err = net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}
	payreq, err := net.Bob.DecodePayReq(ctxt, &lnrpc.PayReqString{PayReq: invoiceResp.PaymentRequest})
	if err != nil {
		t.Fatalf("failed to decode payment request %v", err)
	}
	if len(payreq.RouteHints) != 1 {
		t.Fatalf("expected one routing hint")
	}
	routingHint := payreq.RouteHints[0]
	if len(routingHint.HopHints) != 1 {
		t.Fatalf("expected one hop hint")
	}
	hopHint := routingHint.HopHints[0]
	if hopHint.FeeProportionalMillionths != 1000000 {
		t.Fatalf("wrong FeeProportionalMillionths %v",
			hopHint.FeeProportionalMillionths)
	}
	if hopHint.NodeId != bobPubKey {
		t.Fatalf("wrong NodeId %v",
			hopHint.NodeId)
	}
	if hopHint.ChanId != hintChannel.ToUint64() {
		t.Fatalf("wrong ChanId %v",
			hopHint.ChanId)
	}
	if hopHint.FeeBaseMsat != 1 {
		t.Fatalf("wrong FeeBaseMsat %v",
			hopHint.FeeBaseMsat)
	}
	if hopHint.CltvExpiryDelta != 20 {
		t.Fatalf("wrong CltvExpiryDelta %v",
			hopHint.CltvExpiryDelta)
	}

	closeChannelAndAssert(t, net, net.Alice, chanPoint, false)
}
