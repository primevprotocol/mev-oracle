package updater_test

import (
	"bytes"
	"context"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	preconf "github.com/primevprotocol/contracts-abi/clients/PreConfCommitmentStore"
	"github.com/primevprotocol/mev-oracle/pkg/events"
	"github.com/primevprotocol/mev-oracle/pkg/settler"
	"github.com/primevprotocol/mev-oracle/pkg/updater"
	"golang.org/x/crypto/sha3"
)

func getIdxBytes(idx int64) [32]byte {
	var idxBytes [32]byte
	big.NewInt(idx).FillBytes(idxBytes[:])
	return idxBytes
}

type testHasher struct {
	hasher hash.Hash
}

// NewHasher returns a new testHasher instance.
func NewHasher() *testHasher {
	return &testHasher{hasher: sha3.NewLegacyKeccak256()}
}

// Reset resets the hash state.
func (h *testHasher) Reset() {
	h.hasher.Reset()
}

// Update updates the hash state with the given key and value.
func (h *testHasher) Update(key, val []byte) error {
	h.hasher.Write(key)
	h.hasher.Write(val)
	return nil
}

// Hash returns the hash value.
func (h *testHasher) Hash() common.Hash {
	return common.BytesToHash(h.hasher.Sum(nil))
}

func TestUpdater(t *testing.T) {
	t.Parallel()

	// timestamp of the First block commitment is X
	startTimestamp := time.UnixMilli(1615195200000)
	midTimestamp := startTimestamp.Add(time.Duration(2.5 * float64(time.Second)))
	endTimestamp := startTimestamp.Add(5 * time.Second)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	builderAddr := common.HexToAddress("0xabcd")
	otherBuilderAddr := common.HexToAddress("0xabdc")

	signer := types.NewLondonSigner(big.NewInt(5))
	var txns []*types.Transaction
	for i := 0; i < 10; i++ {
		txns = append(txns, types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
			Nonce:     uint64(i + 1),
			Gas:       1000000,
			Value:     big.NewInt(1),
			GasTipCap: big.NewInt(500),
			GasFeeCap: big.NewInt(500),
		}))
	}

	encCommitments := make([]preconf.PreconfcommitmentstoreEncryptedCommitmentStored, 0)
	commitments := make([]preconf.PreconfcommitmentstoreCommitmentStored, 0)

	for i, txn := range txns {
		idxBytes := getIdxBytes(int64(i))

		encCommitment := preconf.PreconfcommitmentstoreEncryptedCommitmentStored{
			CommitmentIndex:     idxBytes,
			CommitmentDigest:    common.HexToHash(fmt.Sprintf("0x%02d", i)),
			CommitmentSignature: []byte("signature"),
			BlockCommitedAt:     big.NewInt(0),
		}
		commitment := preconf.PreconfcommitmentstoreCommitmentStored{
			CommitmentIndex:     idxBytes,
			TxnHash:             strings.TrimPrefix(txn.Hash().Hex(), "0x"),
			BlockNumber:         5,
			CommitmentHash:      common.HexToHash(fmt.Sprintf("0x%02d", i)),
			CommitmentSignature: []byte("signature"),
			BlockCommitedAt:     big.NewInt(0),
			DecayStartTimeStamp: uint64(startTimestamp.UnixMilli()),
			DecayEndTimeStamp:   uint64(endTimestamp.UnixMilli()),
		}

		if i%2 == 0 {
			encCommitment.Commiter = builderAddr
			commitment.Commiter = builderAddr
			encCommitments = append(encCommitments, encCommitment)
			commitments = append(commitments, commitment)
		} else {
			encCommitment.Commiter = otherBuilderAddr
			commitment.Commiter = otherBuilderAddr
			encCommitments = append(encCommitments, encCommitment)
			commitments = append(commitments, commitment)
		}
	}

	// constructing bundles
	for i := 0; i < 10; i++ {
		idxBytes := getIdxBytes(int64(i + 10))

		bundle := strings.TrimPrefix(txns[i].Hash().Hex(), "0x")
		for j := i + 1; j < 10; j++ {
			bundle += "," + strings.TrimPrefix(txns[j].Hash().Hex(), "0x")
		}

		encCommitment := preconf.PreconfcommitmentstoreEncryptedCommitmentStored{
			CommitmentIndex:     idxBytes,
			Commiter:            builderAddr,
			CommitmentDigest:    common.HexToHash(fmt.Sprintf("0x%02d", i)),
			CommitmentSignature: []byte("signature"),
			BlockCommitedAt:     big.NewInt(0),
		}
		commitment := preconf.PreconfcommitmentstoreCommitmentStored{
			CommitmentIndex:     idxBytes,
			Commiter:            builderAddr,
			TxnHash:             bundle,
			BlockNumber:         5,
			CommitmentHash:      common.HexToHash(fmt.Sprintf("0x%02d", i)),
			CommitmentSignature: []byte("signature"),
			BlockCommitedAt:     big.NewInt(0),
			DecayStartTimeStamp: uint64(startTimestamp.UnixMilli()),
			DecayEndTimeStamp:   uint64(endTimestamp.UnixMilli()),
		}
		encCommitments = append(encCommitments, encCommitment)
		commitments = append(commitments, commitment)
	}

	register := &testWinnerRegister{
		winner: updater.Winner{
			Winner: builderAddr.Bytes(),
			Window: 1,
		},
		settlements: make(chan testSettlement, 1),
		encCommit:   make(chan testEncryptedCommitment, 1),
	}

	l1Client := &testEVMClient{
		blockNum: 5,
		block:    types.NewBlock(&types.Header{}, txns, nil, nil, NewHasher()),
	}

	l2Client := &testEVMClient{
		blockNum: 0,
		block:    types.NewBlock(&types.Header{Time: uint64(midTimestamp.UnixMilli())}, txns, nil, nil, NewHasher()),
	}

	pcABI, err := abi.JSON(strings.NewReader(preconf.PreconfcommitmentstoreABI))
	if err != nil {
		t.Fatal(err)
	}

	evtMgr := &testEventManager{
		pcABI:         &pcABI,
		sub:           &testSub{errC: make(chan error)},
		encHandlerSub: make(chan struct{}),
		cHandlerSub:   make(chan struct{}),
	}

	updtr, err := updater.NewUpdater(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		l1Client,
		l2Client,
		register,
		evtMgr,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := updtr.Start(ctx)

	<-evtMgr.encHandlerSub

	for _, ec := range encCommitments {
		if err := evtMgr.publishEncCommitment(ec); err != nil {
			t.Fatal(err)
		}

		select {
		case <-time.After(5 * time.Second):
			t.Fatal("timeout")
		case enc := <-register.encCommit:
			if !bytes.Equal(enc.commitmentIdx, ec.CommitmentIndex[:]) {
				t.Fatal("wrong commitment index")
			}
			if !bytes.Equal(enc.committer, ec.Commiter.Bytes()) {
				t.Fatal("wrong committer")
			}
			if !bytes.Equal(enc.commitmentHash, ec.CommitmentDigest[:]) {
				t.Fatal("wrong commitment hash")
			}
			if !bytes.Equal(enc.commitmentSignature, ec.CommitmentSignature) {
				t.Fatal("wrong commitment signature")
			}
			if enc.blockNum != 0 {
				t.Fatal("wrong block number")
			}
		}
	}

	<-evtMgr.cHandlerSub

	for _, c := range commitments {
		if err := evtMgr.publishCommitment(c); err != nil {
			t.Fatal(err)
		}

		if c.Commiter.Cmp(otherBuilderAddr) == 0 {
			continue
		}

		select {
		case <-time.After(5 * time.Second):
			t.Fatal("timeout")
		case settlement := <-register.settlements:
			if !bytes.Equal(settlement.commitmentIdx, c.CommitmentIndex[:]) {
				t.Fatal("wrong commitment index")
			}
			if settlement.txHash != c.TxnHash {
				t.Fatal("wrong txn hash")
			}
			if settlement.blockNum != 5 {
				t.Fatal("wrong block number")
			}
			if !bytes.Equal(settlement.builder, c.Commiter.Bytes()) {
				t.Fatal("wrong builder")
			}
			if settlement.amount != 0 {
				t.Fatal("wrong amount")
			}
			if settlement.settlementType != settler.SettlementTypeReward {
				t.Fatal("wrong settlement type")
			}
			if settlement.decayPercentage != 50 {
				t.Fatal("wrong decay percentage")
			}
			if settlement.window != 1 {
				t.Fatal("wrong window")
			}
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestUpdaterBundlesFailure(t *testing.T) {
	t.Parallel()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	startTimestamp := time.UnixMilli(1615195200000)
	midTimestamp := startTimestamp.Add(time.Duration(2.5 * float64(time.Second)))
	endTimestamp := startTimestamp.Add(5 * time.Second)

	builderAddr := common.HexToAddress("0xabcd")

	signer := types.NewLondonSigner(big.NewInt(5))
	var txns []*types.Transaction
	for i := 0; i < 10; i++ {
		txns = append(txns, types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
			Nonce:     uint64(i + 1),
			Gas:       1000000,
			Value:     big.NewInt(1),
			GasTipCap: big.NewInt(500),
			GasFeeCap: big.NewInt(500),
		}))
	}

	commitments := make([]preconf.PreconfcommitmentstoreCommitmentStored, 0)

	// constructing bundles
	for i := 1; i < 10; i++ {
		idxBytes := getIdxBytes(int64(i))

		bundle := txns[i].Hash().Hex()
		for j := 10 - i; j > 0; j-- {
			bundle += "," + txns[j].Hash().Hex()
		}

		commitment := preconf.PreconfcommitmentstoreCommitmentStored{
			CommitmentIndex:     idxBytes,
			Commiter:            builderAddr,
			TxnHash:             bundle,
			BlockNumber:         5,
			CommitmentHash:      common.HexToHash(fmt.Sprintf("0x%02d", i)),
			CommitmentSignature: []byte("signature"),
			BlockCommitedAt:     big.NewInt(0),
			DecayStartTimeStamp: uint64(startTimestamp.UnixMilli()),
			DecayEndTimeStamp:   uint64(endTimestamp.UnixMilli()),
		}

		commitments = append(commitments, commitment)
	}

	register := &testWinnerRegister{
		winner: updater.Winner{
			Winner: builderAddr.Bytes(),
			Window: 1,
		},
		settlements: make(chan testSettlement, 1),
	}

	l1Client := &testEVMClient{
		blockNum: 5,
		block:    types.NewBlock(&types.Header{}, txns, nil, nil, NewHasher()),
	}

	l2Client := &testEVMClient{
		blockNum: 0,
		block:    types.NewBlock(&types.Header{Time: uint64(midTimestamp.UnixMilli())}, txns, nil, nil, NewHasher()),
	}

	pcABI, err := abi.JSON(strings.NewReader(preconf.PreconfcommitmentstoreABI))
	if err != nil {
		t.Fatal(err)
	}

	evtMgr := &testEventManager{
		pcABI:         &pcABI,
		sub:           &testSub{errC: make(chan error)},
		encHandlerSub: make(chan struct{}),
		cHandlerSub:   make(chan struct{}),
	}

	updtr, err := updater.NewUpdater(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		l1Client,
		l2Client,
		register,
		evtMgr,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := updtr.Start(ctx)

	<-evtMgr.cHandlerSub

	for _, c := range commitments {
		if err := evtMgr.publishCommitment(c); err != nil {
			t.Fatal(err)
		}

		select {
		case <-time.After(5 * time.Second):
			t.Fatal("timeout")
		case settlement := <-register.settlements:
			if !bytes.Equal(settlement.commitmentIdx, c.CommitmentIndex[:]) {
				t.Fatal("wrong commitment index")
			}
			if settlement.txHash != c.TxnHash {
				t.Fatal("wrong txn hash")
			}
			if settlement.blockNum != 5 {
				t.Fatal("wrong block number")
			}
			if !bytes.Equal(settlement.builder, c.Commiter.Bytes()) {
				t.Fatal("wrong builder")
			}
			if settlement.amount != 0 {
				t.Fatal("wrong amount")
			}
			if settlement.settlementType != settler.SettlementTypeSlash {
				t.Fatal("wrong settlement type")
			}
			if settlement.decayPercentage != 0 {
				t.Fatal("wrong decay percentage")
			}
			if settlement.window != 1 {
				t.Fatal("wrong window")
			}
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

type testSettlement struct {
	commitmentIdx   []byte
	txHash          string
	blockNum        int64
	builder         []byte
	amount          uint64
	settlementType  settler.SettlementType
	decayPercentage int64
	window          int64
}

type testEncryptedCommitment struct {
	commitmentIdx       []byte
	committer           []byte
	commitmentHash      []byte
	commitmentSignature []byte
	blockNum            int64
}

type testWinnerRegister struct {
	winner      updater.Winner
	settlements chan testSettlement
	encCommit   chan testEncryptedCommitment
}

func (t *testWinnerRegister) IsSettled(ctx context.Context, commitmentIdx []byte) (bool, error) {
	return false, nil
}

func (t *testWinnerRegister) GetWinner(ctx context.Context, blockNum int64) (updater.Winner, error) {
	return t.winner, nil
}

func (t *testWinnerRegister) AddSettlement(
	ctx context.Context,
	commitmentIdx []byte,
	txHash string,
	blockNum int64,
	amount uint64,
	builder []byte,
	_ []byte,
	settlementType settler.SettlementType,
	decayPercentage int64,
	window int64,
) error {
	t.settlements <- testSettlement{
		commitmentIdx:   commitmentIdx,
		txHash:          txHash,
		blockNum:        blockNum,
		amount:          amount,
		builder:         builder,
		settlementType:  settlementType,
		decayPercentage: decayPercentage,
		window:          window,
	}
	return nil
}

func (t *testWinnerRegister) AddEncryptedCommitment(
	ctx context.Context,
	commitmentIdx []byte,
	committer []byte,
	commitmentHash []byte,
	commitmentSignature []byte,
	blockNum int64,
) error {
	t.encCommit <- testEncryptedCommitment{
		commitmentIdx:       commitmentIdx,
		committer:           committer,
		commitmentHash:      commitmentHash,
		commitmentSignature: commitmentSignature,
		blockNum:            blockNum,
	}
	return nil
}

type testEVMClient struct {
	blockNum int64
	block    *types.Block
}

func (t *testEVMClient) BlockByNumber(ctx context.Context, blkNum *big.Int) (*types.Block, error) {
	if blkNum.Int64() == t.blockNum {
		return t.block, nil
	}
	return nil, fmt.Errorf("block %d not found", blkNum.Int64())
}

type testEventManager struct {
	pcABI         *abi.ABI
	encHandler    events.EventHandler
	cHandler      events.EventHandler
	encHandlerSub chan struct{}
	cHandlerSub   chan struct{}
	sub           *testSub
}

type testSub struct {
	errC chan error
}

func (t *testSub) Unsubscribe() {}

func (t *testSub) Err() <-chan error {
	return t.errC
}

func (t *testEventManager) Subscribe(evt events.EventHandler) (events.Subscription, error) {
	switch evt.EventName() {
	case "EncryptedCommitmentStored":
		evt.SetTopicAndContract(t.pcABI.Events["EncryptedCommitmentStored"].ID, t.pcABI)
		t.encHandler = evt
		close(t.encHandlerSub)
	case "CommitmentStored":
		evt.SetTopicAndContract(t.pcABI.Events["CommitmentStored"].ID, t.pcABI)
		t.cHandler = evt
		close(t.cHandlerSub)
	default:
		return nil, fmt.Errorf("event %s not found", evt.EventName())
	}

	return t.sub, nil
}

func (t *testEventManager) publishEncCommitment(ec preconf.PreconfcommitmentstoreEncryptedCommitmentStored) error {
	event := t.pcABI.Events["EncryptedCommitmentStored"]
	buf, err := event.Inputs.NonIndexed().Pack(
		ec.Commiter,
		ec.CommitmentDigest,
		ec.CommitmentSignature,
		ec.BlockCommitedAt,
	)
	if err != nil {
		return err
	}

	commitmentIndex := common.BytesToHash(ec.CommitmentIndex[:])

	// Creating a Log object
	testLog := types.Log{
		Topics: []common.Hash{
			event.ID,        // The first topic is the hash of the event signature
			commitmentIndex, // The next topics are the indexed event parameters
		},
		// Non-indexed parameters are stored in the Data field
		Data: buf,
	}

	return t.encHandler.Handle(testLog)
}

func (t *testEventManager) publishCommitment(c preconf.PreconfcommitmentstoreCommitmentStored) error {
	event := t.pcABI.Events["CommitmentStored"]
	buf, err := event.Inputs.NonIndexed().Pack(
		c.Bidder,
		c.Commiter,
		c.Bid,
		c.BlockNumber,
		c.BidHash,
		c.DecayStartTimeStamp,
		c.DecayEndTimeStamp,
		c.TxnHash,
		c.CommitmentHash,
		c.BidSignature,
		c.CommitmentSignature,
		c.BlockCommitedAt,
		c.SharedSecretKey,
	)
	if err != nil {
		return err
	}

	commitmentIndex := common.BytesToHash(c.CommitmentIndex[:])

	// Creating a Log object
	testLog := types.Log{
		Topics: []common.Hash{
			event.ID,        // The first topic is the hash of the event signature
			commitmentIndex, // The next topics are the indexed event parameters
		},
		// Since there are no non-indexed parameters, Data is empty
		Data: buf,
	}

	return t.cHandler.Handle(testLog)
}
