package chaintracer_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"log"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	repository "github.com/primevprotocol/oracle/pkg/Repository"
	"github.com/primevprotocol/oracle/pkg/chaintracer"
	"github.com/stretchr/testify/assert"
)

func getAuth(privateKey *ecdsa.PrivateKey, chainID *big.Int, client *ethclient.Client, t *testing.T) (opts *bind.TransactOpts) {
	// Set transaction opts
	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, chainID)
	if err != nil {
		t.Error("error creating transaction opts")
	}
	t.Log(client.ChainID(context.Background()))

	// Set nonce (optional)
	nonce, err := client.PendingNonceAt(context.Background(), auth.From)
	if err != nil {
		log.Fatalf("Failed to get nonce: %v", err)
	}
	auth.Nonce = big.NewInt(int64(nonce))

	// Set gas price (optional)
	gasPrice, err := client.SuggestGasPrice(context.Background())
	if err != nil {
		log.Fatalf("Failed to get gas price: %v", err)
	}
	auth.GasPrice = gasPrice

	// Set gas limit (you need to estimate or set a fixed value)
	auth.GasLimit = uint64(300000) // Example value

	return auth
}

type PreConfirmationsContract interface {
	GetCommitmentsByBlockNumber(opts *bind.CallOpts, blockNumber *big.Int) ([][32]byte, error)
	GetTxnHashFromCommitment(opts *bind.CallOpts, commitmentIndex [32]byte) (string, error)
}

type mockPreConfContract struct {
	commitmentsAndTxnHashes map[[32]byte]string
}

func (m *mockPreConfContract) GetCommitmentsByBlockNumber(opts *bind.CallOpts, blockNumber *big.Int) ([][32]byte, error) {
	commitments := [][32]byte{}
	for commitment := range m.commitmentsAndTxnHashes {
		commitments = append(commitments, commitment)
	}
	return commitments, nil
}

func (m *mockPreConfContract) GetTxnHashFromCommitment(opts *bind.CallOpts, commitmentIndex [32]byte) (string, error) {
	return m.commitmentsAndTxnHashes[commitmentIndex], nil
}

func NewMockTracer() chaintracer.Tracer {
	return &mockTracer{
		blockNumberCurrent: 0,
	}
}

type mockTracer struct {
	blockNumberCurrent int64
}

func (d *mockTracer) GetNextBlockNumber(_ context.Context) int64 {
	d.blockNumberCurrent += 1
	return d.blockNumberCurrent
}

func (d *mockTracer) RetrieveDetails() (block *chaintracer.BlockDetails, BlockBuilder string, err error) {
	block = &chaintracer.BlockDetails{
		BlockNumber:  strconv.FormatInt(d.blockNumberCurrent, 10),
		Transactions: []string{},
	}

	for i := 0; i < 200; i++ {
		randomInt, err := rand.Int(rand.Reader, big.NewInt(1000))
		if err != nil {
			panic(err)
		}
		randomBytes := crypto.Keccak256(randomInt.Bytes())
		block.Transactions = append(block.Transactions, hex.EncodeToString(randomBytes))
	}

	sleepDuration, _ := rand.Int(rand.Reader, big.NewInt(12))
	time.Sleep(time.Duration(sleepDuration.Int64()) * time.Second)
	return block, "k builder", nil
}

func TestFilter(t *testing.T) {
	txnFilter := repository.NewTransactionCommitmentFilter(&mockPreConfContract{
		commitmentsAndTxnHashes: map[[32]byte]string{
			{0x02, 0x90, 0x00}: "0xalok",
			{0x02, 0x90, 0x01}: "0xkartik",
			{0x02, 0x90, 0x32}: "0xkant",
			{0x02, 0x90, 0x02}: "0xshawn",
		},
	})
	commitmentChannel, errChannel := txnFilter.RetrieveCommitments(2)
	commit := <-commitmentChannel
	assert.Equal(t, true, commit["0xalok"])
	assert.Equal(t, true, commit["0xkartik"])
	assert.Equal(t, true, commit["0xkant"])
	assert.Equal(t, true, commit["0xshawn"])
	assert.Equal(t, false, commit["0xmurat"])

	assert.Nil(t, <-errChannel)

}
