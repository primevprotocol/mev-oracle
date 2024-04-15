package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	blocktracker "github.com/primevprotocol/contracts-abi/clients/BlockTracker"
	rollupclient "github.com/primevprotocol/contracts-abi/clients/Oracle"
	preconf "github.com/primevprotocol/contracts-abi/clients/PreConfCommitmentStore"
	"github.com/primevprotocol/mev-oracle/pkg/apiserver"
	"github.com/primevprotocol/mev-oracle/pkg/events"
	"github.com/primevprotocol/mev-oracle/pkg/keysigner"
	"github.com/primevprotocol/mev-oracle/pkg/l1Listener"
	"github.com/primevprotocol/mev-oracle/pkg/settler"
	"github.com/primevprotocol/mev-oracle/pkg/store"
	"github.com/primevprotocol/mev-oracle/pkg/transactor"
	"github.com/primevprotocol/mev-oracle/pkg/updater"
)

type Options struct {
	Logger                   *slog.Logger
	KeySigner                keysigner.KeySigner
	HTTPPort                 int
	SettlementRPCUrl         string
	L1RPCUrl                 string
	OracleContractAddr       common.Address
	PreconfContractAddr      common.Address
	BlockTrackerContractAddr common.Address
	PgHost                   string
	PgPort                   int
	PgUser                   string
	PgPassword               string
	PgDbname                 string
	LaggerdMode              int
	OverrideWinners          []string
}

type Node struct {
	logger    *slog.Logger
	waitClose func()
	dbCloser  io.Closer
}

func NewNode(opts *Options) (*Node, error) {
	nd := &Node{logger: opts.Logger}

	db, err := initDB(opts)
	if err != nil {
		opts.Logger.Error("failed initializing DB", "error", err)
		return nil, err
	}
	nd.dbCloser = db

	st, err := store.NewStore(db)
	if err != nil {
		nd.logger.Error("failed initializing store", "error", err)
		return nil, err
	}

	owner := opts.KeySigner.GetAddress()

	settlementClient, err := ethclient.Dial(opts.SettlementRPCUrl)
	if err != nil {
		nd.logger.Error("failed to connect to the settlement layer", "error", err)
		return nil, err
	}

	chainID, err := settlementClient.ChainID(context.Background())
	if err != nil {
		nd.logger.Error("failed getting chain ID", "error", err)
		return nil, err
	}

	l1Client, err := ethclient.Dial(opts.L1RPCUrl)
	if err != nil {
		nd.logger.Error("Failed to connect to the L1 Ethereum client", "error", err)
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	txnMgr, err := transactor.NewContractTransactor(
		ctx,
		settlementClient,
		st,
		owner,
		nd.logger.With("component", "transactor"),
	)
	if err != nil {
		nd.logger.Error("failed to instantiate transactor", "error", err)
		cancel()
		return nil, err
	}

	contracts, err := getContractABIs(opts)
	if err != nil {
		nd.logger.Error("failed to get contract ABIs", "error", err)
		cancel()
		return nil, err
	}

	evtMgr := events.NewListener(
		nd.logger.With("component", "events"),
		settlementClient,
		st,
		contracts,
	)

	evtMgrDone := evtMgr.Start(ctx)

	var listenerL1Client l1Listener.EthClient

	listenerL1Client = l1Client
	if opts.LaggerdMode > 0 {
		listenerL1Client = &laggerdL1Client{EthClient: listenerL1Client, amount: opts.LaggerdMode}
	}

	callOpts := bind.CallOpts{
		Pending: false,
		From:    owner,
		Context: ctx,
	}

	oracleCaller, err := rollupclient.NewOracleCaller(opts.OracleContractAddr, settlementClient)
	if err != nil {
		nd.logger.Error("failed to instantiate oracle caller", "error", err)
		cancel()
		return nil, err
	}

	oracleCallerSession := &rollupclient.OracleCallerSession{
		Contract: oracleCaller,
		CallOpts: callOpts,
	}

	blockTracker, err := blocktracker.NewBlocktrackerTransactor(
		opts.BlockTrackerContractAddr,
		txnMgr,
	)
	if err != nil {
		nd.logger.Error("failed to instantiate block tracker contract", "error", err)
		cancel()
		return nil, err
	}

	oracleTransactor, err := rollupclient.NewOracleTransactor(
		opts.OracleContractAddr,
		txnMgr,
	)
	if err != nil {
		nd.logger.Error("failed to instantiate oracle transactor", "error", err)
		cancel()
		return nil, err
	}

	tOpts, err := opts.KeySigner.GetAuth(chainID)
	if err != nil {
		nd.logger.Error("failed to get auth", "error", err)
		cancel()
		return nil, err
	}

	blockTrackerTransactor := &blocktracker.BlocktrackerTransactorSession{
		Contract:     blockTracker,
		TransactOpts: *tOpts,
	}

	oracleTransactorSession := &rollupclient.OracleTransactorSession{
		Contract:     oracleTransactor,
		TransactOpts: *tOpts,
	}

	if opts.OverrideWinners != nil && len(opts.OverrideWinners) > 0 {
		listenerL1Client = &winnerOverrideL1Client{EthClient: listenerL1Client, winners: opts.OverrideWinners}
		for _, winner := range opts.OverrideWinners {
			err := setBuilderMapping(
				ctx,
				oracleTransactorSession,
				settlementClient,
				winner,
				winner,
			)
			if err != nil {
				nd.logger.Error("failed to set builder mapping", "error", err)
				cancel()
				return nil, err
			}
		}
	}

	l1Lis := l1Listener.NewL1Listener(
		nd.logger.With("component", "l1_listener"),
		listenerL1Client,
		st,
		oracleCallerSession,
		evtMgr,
		blockTrackerTransactor,
	)
	l1LisClosed := l1Lis.Start(ctx)

	updtr, err := updater.NewUpdater(
		nd.logger.With("component", "updater"),
		l1Client,
		settlementClient,
		st,
		evtMgr,
	)
	if err != nil {
		nd.logger.Error("failed to instantiate updater", "error", err)
		cancel()
		return nil, err
	}

	updtrClosed := updtr.Start(ctx)

	settlr := settler.NewSettler(
		nd.logger.With("component", "settler"),
		oracleTransactorSession,
		st,
		evtMgr,
	)
	settlrClosed := settlr.Start(ctx)

	srv := apiserver.New(nd.logger.With("component", "apiserver"))
	srv.RegisterMetricsCollectors(l1Lis.Metrics()...)
	srv.RegisterMetricsCollectors(updtr.Metrics()...)
	srv.RegisterMetricsCollectors(settlr.Metrics()...)

	srvClosed := srv.Start(fmt.Sprintf(":%d", opts.HTTPPort))

	nd.waitClose = func() {
		cancel()

		_ = srv.Stop()

		closeChan := make(chan struct{})
		go func() {
			defer close(closeChan)

			<-l1LisClosed
			<-updtrClosed
			<-settlrClosed
			<-srvClosed
			<-evtMgrDone
		}()

		<-closeChan
	}

	return nd, nil
}

func (n *Node) Close() (err error) {
	defer func() {
		if n.dbCloser != nil {
			if err2 := n.dbCloser.Close(); err2 != nil {
				err = errors.Join(err, err2)
			}
		}
	}()
	workersClosed := make(chan struct{})
	go func() {
		defer close(workersClosed)

		if n.waitClose != nil {
			n.waitClose()
		}
	}()

	select {
	case <-workersClosed:
		n.logger.Info("all workers closed")
		return nil
	case <-time.After(10 * time.Second):
		n.logger.Error("timeout waiting for workers to close")
		return errors.New("timeout waiting for workers to close")
	}
}

func initDB(opts *Options) (db *sql.DB, err error) {
	// Connection string
	psqlInfo := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		opts.PgHost, opts.PgPort, opts.PgUser, opts.PgPassword, opts.PgDbname,
	)

	// Open a connection
	db, err = sql.Open("postgres", psqlInfo)
	if err != nil {
		return nil, err
	}

	// Check the connection
	err = db.Ping()
	if err != nil {
		return nil, err
	}

	return db, err
}

func getContractABIs(opts *Options) (map[common.Address]*abi.ABI, error) {
	abis := make(map[common.Address]*abi.ABI)

	btABI, err := abi.JSON(strings.NewReader(blocktracker.BlocktrackerABI))
	if err != nil {
		return nil, err
	}
	abis[opts.BlockTrackerContractAddr] = &btABI

	pcABI, err := abi.JSON(strings.NewReader(preconf.PreconfcommitmentstoreABI))
	if err != nil {
		return nil, err
	}
	abis[opts.PreconfContractAddr] = &pcABI

	return abis, nil
}

type laggerdL1Client struct {
	l1Listener.EthClient
	amount int
}

func (l *laggerdL1Client) BlockNumber(ctx context.Context) (uint64, error) {
	blkNum, err := l.EthClient.BlockNumber(ctx)
	if err != nil {
		return 0, err
	}

	return blkNum - uint64(l.amount), nil
}

type winnerOverrideL1Client struct {
	l1Listener.EthClient
	winners []string
}

func (w *winnerOverrideL1Client) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	hdr, err := w.EthClient.HeaderByNumber(ctx, number)
	if err != nil {
		return nil, err
	}

	idx := number.Int64() % int64(len(w.winners))
	hdr.Extra = []byte(w.winners[idx])

	return hdr, nil
}

func setBuilderMapping(
	ctx context.Context,
	oracle *rollupclient.OracleTransactorSession,
	client *ethclient.Client,
	builderName string,
	builderAddress string,
) error {
	fmt.Println("Setting builder mapping", builderName, builderAddress)
	txn, err := oracle.AddBuilderAddress(builderName, common.HexToAddress(builderAddress))
	if err != nil {
		return err
	}

	_, err = bind.WaitMined(ctx, client, txn)
	if err != nil {
		return err
	}
	fmt.Println("Builder mapping set", builderName, builderAddress)

	return nil
}
