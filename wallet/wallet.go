package wallet

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/coinset"
	"github.com/btcsuite/btcutil/txsort"
	"github.com/btcsuite/btcwallet/waddrmgr"
	btcwallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	msgBufferSize = 100
)

var (
	// Namespace bucket keys.
	lightningNamespaceKey = []byte("ln-wallet")

	// Error types
	ErrInsufficientFunds = errors.New("not enough available outputs to " +
		"create funding transaction")

	// Which bitcoin network are we using?
	ActiveNetParams = &chaincfg.TestNet3Params
)

type FundingType uint16

const (
	//Use SIGHASH_NOINPUT, assumes CSV
	SIGHASH FundingType = iota

	//Use CSV without reserve
	CSV

	// Use CSV with reserve
	// Reserve is a permanent amount of funds locked and the capacity.
	CSV_RESERVE

	// CLTV with reserve.
	CLTV_RESERVE
)

// completedFundingState...
type completedFundingState struct {
	finalizedReservation *ChannelReservation

	regularTxID wire.ShaHash
}

// initFundingReserveReq...
type initFundingReserveMsg struct {
	fundingAmount btcutil.Amount
	fundingType   FundingType
	minFeeRate    btcutil.Amount

	// TODO(roasbeef): optional reserve for CLTV, etc.

	// Insuffcient funds etc..
	err chan error // Buffered

	resp chan *ChannelReservation // Buffered
}

// FundingReserveCancelMsg...
type fundingReserveCancelMsg struct {
	pendingFundingID uint64

	// Buffered, used for optionally synchronization.
	err  chan error    // Buffered
	done chan struct{} // Buffered
}

// addCounterPartySigsMsg...
type addCounterPartyFundsMsg struct {
	pendingFundingID uint64

	// TODO(roasbeef): Should also carry SPV proofs in we're in SPV mode
	theirInputs        []*wire.TxIn
	theirChangeOutputs []*wire.TxOut
	theirKey           *btcec.PublicKey

	err chan error // Buffered
}

// partiallySignedFundingState...
type partiallySignedFundingState struct {
	// In order of sorted inputs that are ours. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	OurSigs [][]byte

	NormalizedTxID wire.ShaHash
}

// addCounterPartySigsMsg...
type addCounterPartySigsMsg struct {
	pendingFundingID uint64

	// Should be order of sorted inputs that are theirs. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	theirSigs [][]byte

	err chan error // Buffered
}

// FundingCompleteResp...
type finalizedFundingState struct {
	FundingTxId           wire.ShaHash
	NormalizedFundingTXID wire.ShaHash

	CompletedFundingTx *btcutil.Tx
}

// LightningWallet....
// responsible for internal global (from the point of view of a user/node)
// channel state. Requests to modify this state come in via messages over
// channels, same with replies.
// Embedded wallet backed by boltdb...
type LightningWallet struct {
	sync.RWMutex

	db walletdb.DB

	// A namespace within boltdb reserved for ln-based wallet meta-data.
	// TODO(roasbeef): which possible other namespaces are relevant?
	lnNamespace walletdb.Namespace

	*btcwallet.Wallet

	msgChan chan interface{}

	// TODO(roasbeef): zombie garbage collection routine to solve
	// lost-object/starvation problem/attack.
	limboMtx      sync.RWMutex
	nextFundingID uint64
	fundingLimbo  map[uint64]*ChannelReservation

	started  int32
	shutdown int32

	quit chan struct{}

	wg sync.WaitGroup
}

// NewLightningWallet...
// TODO(roasbeef): fin...
func NewLightningWallet(db walletdb.DB, walletPass []byte, create bool) (*LightningWallet, error) {
	lnNamespace, err := db.Namespace(lightningNamespaceKey)
	if err != nil {
		return nil, err
	}

	walletNamespace, err := db.Namespace([]byte("wallet"))
	if err != nil {
		return nil, err
	}

	txNamespace, err := db.Namespace([]byte("tx"))
	if err != nil {
		return nil, err
	}

	if create {
	}

	// TODO(roasbeef): create vs open wallet, use tadge's seed format read func.
	cbs := &waddrmgr.OpenCallbacks{
		ObtainSeed: func() ([]byte, error) {
			return nil, nil
		},
		ObtainPrivatePass: func() ([]byte, error) {
			return nil, nil
		},
	}

	wallet, err := btcwallet.Open(walletPass, ActiveNetParams, db,
		walletNamespace, txNamespace, cbs)
	if err != nil {
		return nil, err
	}

	return &LightningWallet{
		db:            db,
		Wallet:        wallet,
		lnNamespace:   lnNamespace,
		msgChan:       make(chan interface{}, msgBufferSize),
		nextFundingID: 0,
		fundingLimbo:  make(map[uint64]*ChannelReservation),
		quit:          make(chan struct{}),
	}, nil
}

// Start...
func (l *LightningWallet) Start() error {
	// Already started?
	if atomic.AddInt32(&l.started, 1) != 1 {
		return nil
	}

	l.wg.Add(1)
	go l.requestHandler()

	return nil
}

// Stop...
func (l *LightningWallet) Stop() error {
	if atomic.AddInt32(&l.shutdown, 1) != 1 {
		return nil
	}

	close(l.quit)
	l.wg.Wait()
	return nil
}

// requestHandler....
func (l *LightningWallet) requestHandler() {
out:
	for {
		select {
		case m := <-l.msgChan:
			switch msg := m.(type) {
			case *initFundingReserveMsg:
				l.handleFundingReserveRequest(msg)
			case *fundingReserveCancelMsg:
				l.handleFundingCancelRequest(msg)
			case *addCounterPartyFundsMsg:
				l.handleFundingCounterPartyFunds(msg)
			case *addCounterPartySigsMsg:
				l.handleFundingCounterPartySigs(msg)
			}
		case <-l.quit:
			// TODO: do some clean up
			break out
		}
	}

	l.wg.Done()
}

// RequestFundingReservation...
func (l *LightningWallet) RequestChannelReservation(a btcutil.Amount, t FundingType) (*ChannelReservation, error) {
	errChan := make(chan error, 1)
	respChan := make(chan *ChannelReservation, 1)

	l.msgChan <- &initFundingReserveMsg{
		fundingAmount: a,
		fundingType:   t,
		err:           errChan,
		resp:          respChan,
	}

	return <-respChan, <-errChan
}

// handleFundingReserveRequest...
func (l *LightningWallet) handleFundingReserveRequest(req *initFundingReserveMsg) {
	// Create a limbo and record entry for this newly pending funding request.
	l.limboMtx.Lock()

	id := l.nextFundingID
	partialState := newChannelReservation(req.fundingType, req.fundingAmount, req.minFeeRate, l, id)
	l.nextFundingID++
	l.fundingLimbo[id] = partialState

	l.limboMtx.Unlock()

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	partialState.Lock()
	defer partialState.Unlock()

	// Find all unlocked unspent outputs with greater than 6 confirmations.
	maxConfs := ^int32(0)
	unspentOutputs, err := l.ListUnspent(6, maxConfs, nil)
	if err != nil {
		req.err <- err
		return
	}

	// Convert the outputs to coins for coin selection below.
	coins, err := outputsToCoins(unspentOutputs)
	if err != nil {
		req.err <- err
		return
	}

	// Peform coin selection over our available, unlocked unspent outputs
	// in order to find enough coins to meet the funding amount requirements.
	//
	// TODO(roasbeef): Should extend coinset with optimal coin selection
	// heuristics for our use case.
	// TODO(roasbeef): factor in fees..
	// NOTE: this current selection assumes "priority" is still a thing.
	selector := &coinset.MaxValueAgeCoinSelector{
		MaxInputs:       10,
		MinChangeAmount: 10000,
	}
	selectedCoins, err := selector.CoinSelect(req.fundingAmount, coins)

	// Lock the selected coins. These coins are now "reserved", this
	// prevents concurrent funding requests from referring to and this
	// double-spending the same set of coins.
	partialState.ourInputs = make([]*wire.TxIn, len(selectedCoins.Coins()))
	for i, coin := range selectedCoins.Coins() {
		txout := wire.NewOutPoint(coin.Hash(), coin.Index())
		l.LockOutpoint(*txout)

		// Empty sig script, we'll actually sign if this reservation is
		// queued up to be completed (the other side accepts).
		outPoint := wire.NewOutPoint(coin.Hash(), coin.Index())
		partialState.ourInputs[i] = wire.NewTxIn(outPoint, nil)
	}

	// Create some possibly neccessary change outputs.
	selectedTotalValue := coinset.NewCoinSet(coins).TotalValue()
	partialState.ourChange = make([]*wire.TxOut, 0, len(selectedCoins.Coins()))
	if selectedTotalValue > req.fundingAmount {
		// Change is necessary. Query for an available change address to
		// send the remainder to.
		changeAmount := selectedTotalValue - req.fundingAmount
		changeAddr, err := l.NewChangeAddress(waddrmgr.DefaultAccountNum)
		if err != nil {
			req.err <- err
			return
		}

		partialState.ourChange = append(partialState.ourChange,
			wire.NewTxOut(int64(changeAmount), changeAddr.ScriptAddress()))
	}

	// TODO(roasbeef): re-calculate fees here to minFeePerKB, may need more inputs

	multiSigKey, err := l.getNextMultiSigKey()
	if err != nil {
		req.err <- err
		return
	}

	partialState.ourKey = multiSigKey

	// Funding reservation request succesfully handled. The funding inputs
	// will be marked as unavailable until the reservation is either
	// completed, or cancecled.
	req.err <- nil
}

// handleFundingReserveCancel...
func (l *LightningWallet) handleFundingCancelRequest(req *fundingReserveCancelMsg) {
	// TODO(roasbeef): holding lock too long
	l.limboMtx.Lock()
	defer l.limboMtx.Unlock()

	pendingReservation, ok := l.fundingLimbo[req.pendingFundingID]
	if !ok {
		// TODO(roasbeef): make new error, "unkown funding state" or something
		req.err <- fmt.Errorf("attempted to cancel non-existant funding state")
		req.done <- struct{}{}
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Mark all previously locked outpoints as usuable for future funding
	// requests.
	for _, unusedInput := range pendingReservation.ourInputs {
		l.UnlockOutpoint(unusedInput.PreviousOutPoint)
	}

	// TODO(roasbeef): is it even worth it to keep track of unsed keys?

	// TODO(roasbeef): Is it possible to mark the unused change also as
	// available?

	delete(l.fundingLimbo, req.pendingFundingID)

	req.err <- nil
	req.done <- struct{}{}
}

// handleFundingCounterPartyFunds...
func (l *LightningWallet) handleFundingCounterPartyFunds(req *addCounterPartyFundsMsg) {
	l.RLock()
	pendingReservation, ok := l.fundingLimbo[req.pendingFundingID]
	l.RUnlock()
	if !ok {
		req.err <- fmt.Errorf("attempted to update non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Create a blank, fresh transaction. Soon to be a complete funding
	// transaction which will allow opening a lightning channel.
	pendingReservation.fundingTx = wire.NewMsgTx()

	// First, add all multi-party inputs to the transaction
	// TODO(roasbeef); handle case that tx doesn't exist, fake input
	// TODO(roasbeef): validate SPV proof from other side if in SPV mode.
	for _, ourInput := range pendingReservation.ourInputs {
		pendingReservation.fundingTx.AddTxIn(ourInput)
	}
	pendingReservation.theirInputs = req.theirInputs
	for _, theirInput := range pendingReservation.theirInputs {
		pendingReservation.fundingTx.AddTxIn(theirInput)
	}

	// Next, add all multi-party outputs to the transaction. This includes
	// change outputs for both side.
	for _, ourChangeOutput := range pendingReservation.ourChange {
		pendingReservation.fundingTx.AddTxOut(ourChangeOutput)
	}
	pendingReservation.theirChange = req.theirChangeOutputs
	for _, theirChangeOutput := range pendingReservation.theirChange {
		pendingReservation.fundingTx.AddTxOut(theirChangeOutput)
	}

	// Finally, add the 2-of-2 multi-sig output which will set up the lightning
	// channel. TODO(roasbeef): Cannonical sorting of keys here?
	keys := make([]*btcutil.AddressPubKey, 2)
	ourKey := pendingReservation.ourKey.PubKey().SerializeCompressed()
	keys[0], _ = btcutil.NewAddressPubKey(ourKey, ActiveNetParams)
	pendingReservation.theirKey = req.theirKey
	keys[1], _ = btcutil.NewAddressPubKey(pendingReservation.theirKey.SerializeCompressed(), ActiveNetParams)
	multiSigScript, err := txscript.MultiSigScript(keys, 2)
	if err != nil {
		req.err <- err
		return
	}
	multiSigOut := wire.NewTxOut(int64(pendingReservation.FundingAmount),
		multiSigScript)
	pendingReservation.fundingTx.AddTxOut(multiSigOut)

	// Sort the transaction. Since both side agree to a cannonical
	// ordering, by sorting we no longer need to send the entire
	// transaction. Only signatures will be exchanged.
	txsort.InPlaceSort(pendingReservation.fundingTx)

	// Now that the transaction has been cannonically sorted, compute the
	// normalized transation ID before we attach our signatures.
	// TODO(roasbeef): this isn't the normalized txid, this isn't recursive
	pendingReservation.normalizedTxID = pendingReservation.fundingTx.TxSha()

	// Now, sign all inputs that are ours, collecting the signatures in
	// order of the inputs.
	pendingReservation.ourSigs = make([][]byte, len(pendingReservation.ourInputs))
	for i, txIn := range pendingReservation.fundingTx.TxIn {
		// Does the wallet know about the txin?
		txDetail, _ := l.TxStore.TxDetails(&txIn.PreviousOutPoint.Hash)
		if txDetail == nil {
			continue
		}

		// Is this our txin? TODO(roasbeef): assumes all inputs are P2PKH...
		prevIndex := txIn.PreviousOutPoint.Index
		prevOut := txDetail.TxRecord.MsgTx.TxOut[prevIndex]
		_, addrs, _, _ := txscript.ExtractPkScriptAddrs(prevOut.PkScript, ActiveNetParams)
		apkh, ok := addrs[0].(*btcutil.AddressPubKeyHash)
		if !ok {
			req.err <- btcwallet.ErrUnsupportedTransactionType
			return
		}

		ai, err := l.Manager.Address(apkh)
		if err != nil {
			req.err <- fmt.Errorf("cannot get address info: %v", err)
			return
		}
		pka := ai.(waddrmgr.ManagedPubKeyAddress)
		privkey, err := pka.PrivKey()
		if err != nil {
			req.err <- fmt.Errorf("cannot get private key: %v", err)
			return
		}

		sigscript, err := txscript.SignatureScript(pendingReservation.fundingTx, i,
			prevOut.PkScript, txscript.SigHashAll, privkey,
			ai.Compressed())
		if err != nil {
			req.err <- fmt.Errorf("cannot create sigscript: %s", err)
			return
		}

		pendingReservation.fundingTx.TxIn[i].SignatureScript = sigscript
		pendingReservation.ourSigs[i] = sigscript
	}

	req.err <- nil
}

// handleFundingCounterPartySigs...
func (l *LightningWallet) handleFundingCounterPartySigs(msg *addCounterPartySigsMsg) {
	l.limboMtx.RLock()
	pendingReservation, ok := l.fundingLimbo[msg.pendingFundingID]
	l.limboMtx.RUnlock()
	if !ok {
		msg.err <- fmt.Errorf("attempted to update non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Now we can complete the funding transaction by adding their
	// signatures to their inputs.
	i := 0
	pendingReservation.theirSigs = msg.theirSigs
	for _, txin := range pendingReservation.fundingTx.TxIn {
		if txin.SignatureScript == nil {
			// TODO(roasbeef): use txscript.Engine to make sure each sig is
			// valid, txn complete.
			txin.SignatureScript = pendingReservation.theirSigs[i]
			i++
		}
	}

	// TODO(roasbeef): write the funding transaction to disk
	// Now that all signatures are in place and valid record the regular txid
	//completedReservation := &finalizedFundingState{
	//	FundingTxId:           pendingReservation.fundingTx.TxSha(),
	//	NormalizedFundingTXID: pendingReservation.normalizedTxID,
	//	CompletedFundingTx:    btcutil.NewTx(pendingReservation.fundingTx),
	//}

	pendingReservation.completedFundingTx = btcutil.NewTx(pendingReservation.fundingTx)

	l.limboMtx.Lock()
	delete(l.fundingLimbo, pendingReservation.reservationID)
	l.limboMtx.Unlock()

	msg.err <- nil
}

// nextMultiSigKey...
// TODO(roasbeef): on shutdown, write state of pending keys, then read back?
func (l *LightningWallet) getNextMultiSigKey() (*btcec.PrivateKey, error) {
	nextAddr, err := l.Manager.NextExternalAddresses(waddrmgr.DefaultAccountNum, 1)
	if err != nil {
		return nil, err
	}

	pkAddr := nextAddr[0].(waddrmgr.ManagedPubKeyAddress)

	return pkAddr.PrivKey()
}