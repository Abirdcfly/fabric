/*
Copyright IBM Corp. 2016 All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package kvledger

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric-protos-go/ledger/queryresult"
	"github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/bccsp/sw"
	configtxtest "github.com/hyperledger/fabric/common/configtx/test"
	"github.com/hyperledger/fabric/common/ledger/blkstorage"
	"github.com/hyperledger/fabric/common/ledger/dataformat"
	"github.com/hyperledger/fabric/common/ledger/testutil"
	"github.com/hyperledger/fabric/common/ledger/util/leveldbhelper"
	"github.com/hyperledger/fabric/common/metrics/disabled"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/core/ledger"
	lgr "github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/ledger/kvledger/msgs"
	"github.com/hyperledger/fabric/core/ledger/mock"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/stretchr/testify/require"
)

func TestLedgerProvider(t *testing.T) {
	testcases := []struct {
		enableHistoryDB bool
	}{
		{
			enableHistoryDB: true,
		},
		{
			enableHistoryDB: false,
		},
	}

	for i, tc := range testcases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			testLedgerProvider(t, tc.enableHistoryDB)
		})
	}
}

func testLedgerProvider(t *testing.T, enableHistoryDB bool) {
	conf, cleanup := testConfig(t)
	conf.HistoryDBConfig.Enabled = enableHistoryDB
	defer cleanup()
	provider := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	numLedgers := 10
	existingLedgerIDs, err := provider.List()
	require.NoError(t, err)
	require.Len(t, existingLedgerIDs, 0)
	genesisBlocks := make([]*common.Block, numLedgers)
	for i := 0; i < numLedgers; i++ {
		genesisBlock, _ := configtxtest.MakeGenesisBlock(constructTestLedgerID(i))
		genesisBlocks[i] = genesisBlock
		_, err := provider.CreateFromGenesisBlock(genesisBlock)
		require.NoError(t, err)
	}
	existingLedgerIDs, err = provider.List()
	require.NoError(t, err)
	require.Len(t, existingLedgerIDs, numLedgers)

	// verify formatKey is present in idStore
	s := provider.idStore
	val, err := s.db.Get(formatKey)
	require.NoError(t, err)
	require.Equal(t, []byte(dataformat.CurrentFormat), val)

	provider.Close()

	provider = testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	defer provider.Close()
	ledgerIds, _ := provider.List()
	require.Len(t, ledgerIds, numLedgers)
	for i := 0; i < numLedgers; i++ {
		require.Equal(t, constructTestLedgerID(i), ledgerIds[i])
	}
	for i := 0; i < numLedgers; i++ {
		ledgerid := constructTestLedgerID(i)
		status, _ := provider.Exists(ledgerid)
		require.True(t, status)
		ledger, err := provider.Open(ledgerid)
		require.NoError(t, err)
		bcInfo, err := ledger.GetBlockchainInfo()
		ledger.Close()
		require.NoError(t, err)
		require.Equal(t, uint64(1), bcInfo.Height)

		// check that ledger metadata keys were persisted in idStore with active status
		s := provider.idStore
		val, err := s.db.Get(metadataKey(ledgerid))
		require.NoError(t, err)
		metadata := &msgs.LedgerMetadata{}
		require.NoError(t, proto.Unmarshal(val, metadata))
		require.Equal(t, msgs.Status_ACTIVE, metadata.Status)
	}
	gb, _ := configtxtest.MakeGenesisBlock(constructTestLedgerID(2))
	_, err = provider.CreateFromGenesisBlock(gb)
	require.EqualError(t, err, "ledger [ledger_000002] already exists with state [ACTIVE]")

	status, err := provider.Exists(constructTestLedgerID(numLedgers))
	require.NoError(t, err, "Failed to check for ledger existence")
	require.Equal(t, status, false)

	_, err = provider.Open(constructTestLedgerID(numLedgers))
	require.EqualError(t, err, "cannot open ledger [ledger_000010], ledger does not exist")
}

func TestGetActiveLedgerIDsIteratorError(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	provider := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})

	for i := 0; i < 2; i++ {
		genesisBlock, _ := configtxtest.MakeGenesisBlock(constructTestLedgerID(i))
		provider.CreateFromGenesisBlock(genesisBlock)
	}

	// close provider to trigger db error
	provider.Close()
	_, err := provider.idStore.getActiveLedgerIDs()
	require.EqualError(t, err, "error getting ledger ids from idStore: leveldb: closed")
}

func TestLedgerMetataDataUnmarshalError(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	provider := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	defer provider.Close()

	ledgerID := constructTestLedgerID(0)
	genesisBlock, _ := configtxtest.MakeGenesisBlock(ledgerID)
	provider.CreateFromGenesisBlock(genesisBlock)

	// put invalid bytes for the metatdata key
	provider.idStore.db.Put(metadataKey(ledgerID), []byte("invalid"), true)

	_, err := provider.List()
	require.EqualError(t, err, "error unmarshalling ledger metadata: unexpected EOF")

	_, err = provider.Open(ledgerID)
	require.EqualError(t, err, "error unmarshalling ledger metadata: unexpected EOF")
}

func TestNewProviderIdStoreFormatError(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()

	require.NoError(t, testutil.Unzip("tests/testdata/v11/sample_ledgers/ledgersData.zip", conf.RootFSPath, false))

	// NewProvider fails because ledgerProvider (idStore) has old format
	_, err := NewProvider(
		&lgr.Initializer{
			DeployedChaincodeInfoProvider: &mock.DeployedChaincodeInfoProvider{},
			MetricsProvider:               &disabled.Provider{},
			Config:                        conf,
		},
	)
	require.EqualError(t, err, fmt.Sprintf("unexpected format. db info = [leveldb for channel-IDs at [%s]], data format = [], expected format = [2.0]", LedgerProviderPath(conf.RootFSPath)))
}

func TestUpgradeIDStoreFormatDBError(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	provider := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	provider.Close()

	err := provider.idStore.upgradeFormat()
	dbPath := LedgerProviderPath(conf.RootFSPath)
	require.EqualError(t, err, fmt.Sprintf("error while trying to see if the leveldb at path [%s] is empty: leveldb: closed", dbPath))
}

func TestCheckUpgradeEligibilityV1x(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	dbPath := LedgerProviderPath(conf.RootFSPath)
	db := leveldbhelper.CreateDB(&leveldbhelper.Conf{DBPath: dbPath})
	idStore := &idStore{db, dbPath}
	db.Open()
	defer db.Close()

	// write a tmpKey so that idStore is not be empty
	err := idStore.db.Put([]byte("tmpKey"), []byte("tmpValue"), true)
	require.NoError(t, err)

	eligible, err := idStore.checkUpgradeEligibility()
	require.NoError(t, err)
	require.True(t, eligible)
}

func TestCheckUpgradeEligibilityCurrentVersion(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	dbPath := LedgerProviderPath(conf.RootFSPath)
	db := leveldbhelper.CreateDB(&leveldbhelper.Conf{DBPath: dbPath})
	idStore := &idStore{db, dbPath}
	db.Open()
	defer db.Close()

	err := idStore.db.Put(formatKey, []byte(dataformat.CurrentFormat), true)
	require.NoError(t, err)

	eligible, err := idStore.checkUpgradeEligibility()
	require.NoError(t, err)
	require.False(t, eligible)
}

func TestCheckUpgradeEligibilityBadFormat(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	dbPath := LedgerProviderPath(conf.RootFSPath)
	db := leveldbhelper.CreateDB(&leveldbhelper.Conf{DBPath: dbPath})
	idStore := &idStore{db, dbPath}
	db.Open()
	defer db.Close()

	err := idStore.db.Put(formatKey, []byte("x.0"), true)
	require.NoError(t, err)

	expectedErr := &dataformat.ErrFormatMismatch{
		ExpectedFormat: dataformat.PreviousFormat,
		Format:         "x.0",
		DBInfo:         fmt.Sprintf("leveldb for channel-IDs at [%s]", LedgerProviderPath(conf.RootFSPath)),
	}
	eligible, err := idStore.checkUpgradeEligibility()
	require.EqualError(t, err, expectedErr.Error())
	require.False(t, eligible)
}

func TestCheckUpgradeEligibilityEmptyDB(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	dbPath := LedgerProviderPath(conf.RootFSPath)
	db := leveldbhelper.CreateDB(&leveldbhelper.Conf{DBPath: dbPath})
	idStore := &idStore{db, dbPath}
	db.Open()
	defer db.Close()

	eligible, err := idStore.checkUpgradeEligibility()
	require.NoError(t, err)
	require.False(t, eligible)
}

func TestDeletionOfUnderConstructionLedgersAtStart(t *testing.T) {
	testcases := []struct {
		enableHistoryDB               bool
		mimicCrashAfterLedgerCreation bool
	}{
		{
			enableHistoryDB:               true,
			mimicCrashAfterLedgerCreation: true,
		},
		{
			enableHistoryDB:               false,
			mimicCrashAfterLedgerCreation: true,
		},
		{
			enableHistoryDB:               true,
			mimicCrashAfterLedgerCreation: false,
		},
		{
			enableHistoryDB:               false,
			mimicCrashAfterLedgerCreation: false,
		},
	}

	for i, tc := range testcases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			testDeletionOfUnderConstructionLedgersAtStart(t, tc.enableHistoryDB, tc.mimicCrashAfterLedgerCreation)
		})
	}
}

func testDeletionOfUnderConstructionLedgersAtStart(t *testing.T, enableHistoryDB, mimicCrashAfterLedgerCreation bool) {
	conf, cleanup := testConfig(t)
	conf.HistoryDBConfig.Enabled = enableHistoryDB
	defer cleanup()
	provider := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	idStore := provider.idStore
	ledgerID := "testLedger"
	defer func() {
		provider.Close()
	}()

	switch mimicCrashAfterLedgerCreation {
	case false:
		idStore.createLedgerID(ledgerID, &msgs.LedgerMetadata{
			Status: msgs.Status_UNDER_CONSTRUCTION,
		})
	case true:
		genesisBlock, err := configtxtest.MakeGenesisBlock(ledgerID)
		require.NoError(t, err)
		_, err = provider.CreateFromGenesisBlock(genesisBlock)
		require.NoError(t, err)
		m, err := provider.idStore.getLedgerMetadata(ledgerID)
		require.NoError(t, err)
		require.Equal(t, msgs.Status_ACTIVE, m.Status)
		// mimic a situation that a crash happens after ledger creation but before changing the UNDER_CONSTRUCTION status
		// to Status_ACTIVE
		provider.idStore.updateLedgerStatus(ledgerID, msgs.Status_UNDER_CONSTRUCTION)
	}
	provider.Close()
	// construct a new provider to invoke recovery
	provider = testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	exists, err := provider.Exists(ledgerID)
	require.NoError(t, err)
	require.False(t, exists)
	m, err := provider.idStore.getLedgerMetadata(ledgerID)
	require.NoError(t, err)
	require.Nil(t, m)
}

func TestLedgerCreationFailure(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	provider := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	ledgerID := "testLedger"
	defer func() {
		provider.Close()
	}()

	genesisBlock, err := configtxtest.MakeGenesisBlock(ledgerID)
	require.NoError(t, err)
	genesisBlock.Header.Number = 1 // should cause an error during ledger creation
	_, err = provider.CreateFromGenesisBlock(genesisBlock)
	require.EqualError(t, err, "Expected block number=0, received block number=1")

	verifyLedgerDoesNotExist(t, provider, ledgerID)
}

func TestLedgerCreationFailureDuringLedgerDeletion(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	provider := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	ledgerID := "testLedger"
	defer func() {
		provider.Close()
	}()

	genesisBlock, err := configtxtest.MakeGenesisBlock(ledgerID)
	require.NoError(t, err)
	genesisBlock.Header.Number = 1 // should cause an error during ledger creation

	provider.dbProvider.Close()
	_, err = provider.CreateFromGenesisBlock(genesisBlock)
	require.Contains(t, err.Error(), "Expected block number=0, received block number=1: error while deleting data from ledger [testLedger]")

	verifyLedgerIDExists(t, provider, ledgerID, msgs.Status_UNDER_CONSTRUCTION)
}

func TestMultipleLedgerBasicRW(t *testing.T) {
	conf, cleanup := testConfig(t)
	defer cleanup()
	provider1 := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	defer provider1.Close()

	numLedgers := 10
	ledgers := make([]lgr.PeerLedger, numLedgers)
	for i := 0; i < numLedgers; i++ {
		bg, gb := testutil.NewBlockGenerator(t, constructTestLedgerID(i), false)
		l, err := provider1.CreateFromGenesisBlock(gb)
		require.NoError(t, err)
		ledgers[i] = l
		txid := util.GenerateUUID()
		s, _ := l.NewTxSimulator(txid)
		err = s.SetState("ns", "testKey", []byte(fmt.Sprintf("testValue_%d", i)))
		s.Done()
		require.NoError(t, err)
		res, err := s.GetTxSimulationResults()
		require.NoError(t, err)
		pubSimBytes, _ := res.GetPubSimulationBytes()
		b := bg.NextBlock([][]byte{pubSimBytes})
		err = l.CommitLegacy(&lgr.BlockAndPvtData{Block: b}, &ledger.CommitOptions{})
		l.Close()
		require.NoError(t, err)
	}

	provider1.Close()

	provider2 := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	defer provider2.Close()
	ledgers = make([]lgr.PeerLedger, numLedgers)
	for i := 0; i < numLedgers; i++ {
		l, err := provider2.Open(constructTestLedgerID(i))
		require.NoError(t, err)
		ledgers[i] = l
	}

	for i, l := range ledgers {
		q, _ := l.NewQueryExecutor()
		val, err := q.GetState("ns", "testKey")
		q.Done()
		require.NoError(t, err)
		require.Equal(t, []byte(fmt.Sprintf("testValue_%d", i)), val)
		l.Close()
	}
}

func TestLedgerBackup(t *testing.T) {
	ledgerid := "TestLedger"
	basePath, err := ioutil.TempDir("", "kvledger")
	require.NoError(t, err, "Failed to create ledger directory")
	defer os.RemoveAll(basePath)
	originalPath := filepath.Join(basePath, "kvledger1")
	restorePath := filepath.Join(basePath, "kvledger2")

	// create and populate a ledger in the original environment
	origConf := &lgr.Config{
		RootFSPath:    originalPath,
		StateDBConfig: &lgr.StateDBConfig{},
		PrivateDataConfig: &lgr.PrivateDataConfig{
			MaxBatchSize:    5000,
			BatchesInterval: 1000,
			PurgeInterval:   100,
		},
		HistoryDBConfig: &lgr.HistoryDBConfig{
			Enabled: true,
		},
		SnapshotsConfig: &lgr.SnapshotsConfig{
			RootDir: filepath.Join(originalPath, "snapshots"),
		},
	}
	provider := testutilNewProvider(origConf, t, &mock.DeployedChaincodeInfoProvider{})
	bg, gb := testutil.NewBlockGenerator(t, ledgerid, false)
	gbHash := protoutil.BlockHeaderHash(gb.Header)
	ledger, _ := provider.CreateFromGenesisBlock(gb)

	txid := util.GenerateUUID()
	simulator, _ := ledger.NewTxSimulator(txid)
	simulator.SetState("ns1", "key1", []byte("value1"))
	simulator.SetState("ns1", "key2", []byte("value2"))
	simulator.SetState("ns1", "key3", []byte("value3"))
	simulator.Done()
	simRes, _ := simulator.GetTxSimulationResults()
	pubSimBytes, _ := simRes.GetPubSimulationBytes()
	block1 := bg.NextBlock([][]byte{pubSimBytes})
	ledger.CommitLegacy(&lgr.BlockAndPvtData{Block: block1}, &lgr.CommitOptions{})

	txid = util.GenerateUUID()
	simulator, _ = ledger.NewTxSimulator(txid)
	simulator.SetState("ns1", "key1", []byte("value4"))
	simulator.SetState("ns1", "key2", []byte("value5"))
	simulator.SetState("ns1", "key3", []byte("value6"))
	simulator.Done()
	simRes, _ = simulator.GetTxSimulationResults()
	pubSimBytes, _ = simRes.GetPubSimulationBytes()
	block2 := bg.NextBlock([][]byte{pubSimBytes})
	ledger.CommitLegacy(&lgr.BlockAndPvtData{Block: block2}, &lgr.CommitOptions{})

	ledger.Close()
	provider.Close()

	// remove the statedb, historydb, and block indexes (they are supposed to be auto created during opening of an existing ledger)
	// and rename the originalPath to restorePath
	require.NoError(t, os.RemoveAll(StateDBPath(originalPath)))
	require.NoError(t, os.RemoveAll(HistoryDBPath(originalPath)))
	require.NoError(t, os.RemoveAll(filepath.Join(BlockStorePath(originalPath), blkstorage.IndexDir)))
	require.NoError(t, os.Rename(originalPath, restorePath))

	// Instantiate the ledger from restore environment and this should behave exactly as it would have in the original environment
	restoreConf := &lgr.Config{
		RootFSPath:    restorePath,
		StateDBConfig: &lgr.StateDBConfig{},
		PrivateDataConfig: &lgr.PrivateDataConfig{
			MaxBatchSize:    5000,
			BatchesInterval: 1000,
			PurgeInterval:   100,
		},
		HistoryDBConfig: &lgr.HistoryDBConfig{
			Enabled: true,
		},
		SnapshotsConfig: &lgr.SnapshotsConfig{
			RootDir: filepath.Join(restorePath, "snapshots"),
		},
	}
	provider = testutilNewProvider(restoreConf, t, &mock.DeployedChaincodeInfoProvider{})
	defer provider.Close()

	_, err = provider.CreateFromGenesisBlock(gb)
	require.EqualError(t, err, "ledger [TestLedger] already exists with state [ACTIVE]")

	ledger, err = provider.Open(ledgerid)
	require.NoError(t, err)
	defer ledger.Close()

	block1Hash := protoutil.BlockHeaderHash(block1.Header)
	block2Hash := protoutil.BlockHeaderHash(block2.Header)
	bcInfo, _ := ledger.GetBlockchainInfo()
	require.Equal(t, &common.BlockchainInfo{
		Height: 3, CurrentBlockHash: block2Hash, PreviousBlockHash: block1Hash,
	}, bcInfo)

	b0, _ := ledger.GetBlockByHash(gbHash)
	require.True(t, proto.Equal(b0, gb), "proto messages are not equal")

	b1, _ := ledger.GetBlockByHash(block1Hash)
	require.True(t, proto.Equal(b1, block1), "proto messages are not equal")

	b2, _ := ledger.GetBlockByHash(block2Hash)
	require.True(t, proto.Equal(b2, block2), "proto messages are not equal")

	b0, _ = ledger.GetBlockByNumber(0)
	require.True(t, proto.Equal(b0, gb), "proto messages are not equal")

	b1, _ = ledger.GetBlockByNumber(1)
	require.True(t, proto.Equal(b1, block1), "proto messages are not equal")

	b2, _ = ledger.GetBlockByNumber(2)
	require.True(t, proto.Equal(b2, block2), "proto messages are not equal")

	// get the tran id from the 2nd block, then use it to test GetTransactionByID()
	txEnvBytes2 := block1.Data.Data[0]
	txEnv2, err := protoutil.GetEnvelopeFromBlock(txEnvBytes2)
	require.NoError(t, err, "Error upon GetEnvelopeFromBlock")
	payload2, err := protoutil.UnmarshalPayload(txEnv2.Payload)
	require.NoError(t, err, "Error upon GetPayload")
	chdr, err := protoutil.UnmarshalChannelHeader(payload2.Header.ChannelHeader)
	require.NoError(t, err, "Error upon GetChannelHeaderFromBytes")
	txID2 := chdr.TxId
	processedTran2, err := ledger.GetTransactionByID(txID2)
	require.NoError(t, err, "Error upon GetTransactionByID")
	// get the tran envelope from the retrieved ProcessedTransaction
	retrievedTxEnv2 := processedTran2.TransactionEnvelope
	require.Equal(t, txEnv2, retrievedTxEnv2)

	qe, _ := ledger.NewQueryExecutor()
	value1, _ := qe.GetState("ns1", "key1")
	require.Equal(t, []byte("value4"), value1)

	hqe, err := ledger.NewHistoryQueryExecutor()
	require.NoError(t, err)
	itr, err := hqe.GetHistoryForKey("ns1", "key1")
	require.NoError(t, err)
	defer itr.Close()

	result1, err := itr.Next()
	require.NoError(t, err)
	require.Equal(t, []byte("value4"), result1.(*queryresult.KeyModification).Value)
	result2, err := itr.Next()
	require.NoError(t, err)
	require.Equal(t, []byte("value1"), result2.(*queryresult.KeyModification).Value)
}

func constructTestLedgerID(i int) string {
	return fmt.Sprintf("ledger_%06d", i)
}

func testConfig(t *testing.T) (conf *lgr.Config, cleanup func()) {
	path, err := ioutil.TempDir("", "kvledger")
	require.NoError(t, err, "Failed to create test ledger directory")
	conf = &lgr.Config{
		RootFSPath:    path,
		StateDBConfig: &lgr.StateDBConfig{},
		PrivateDataConfig: &lgr.PrivateDataConfig{
			MaxBatchSize:    5000,
			BatchesInterval: 1000,
			PurgeInterval:   100,
		},
		HistoryDBConfig: &lgr.HistoryDBConfig{
			Enabled: true,
		},
		SnapshotsConfig: &lgr.SnapshotsConfig{
			RootDir: filepath.Join(path, "snapshots"),
		},
	}
	cleanup = func() {
		os.RemoveAll(path)
	}

	return conf, cleanup
}

func testutilNewProvider(conf *lgr.Config, t *testing.T, ccInfoProvider *mock.DeployedChaincodeInfoProvider) *Provider {
	cryptoProvider, err := sw.NewDefaultSecurityLevelWithKeystore(sw.NewDummyKeyStore())
	require.NoError(t, err)

	provider, err := NewProvider(
		&lgr.Initializer{
			DeployedChaincodeInfoProvider:   ccInfoProvider,
			MetricsProvider:                 &disabled.Provider{},
			Config:                          conf,
			HashProvider:                    cryptoProvider,
			HealthCheckRegistry:             &mock.HealthCheckRegistry{},
			ChaincodeLifecycleEventProvider: &mock.ChaincodeLifecycleEventProvider{},
		},
	)
	require.NoError(t, err, "Failed to create new Provider")
	return provider
}

type nsCollBtlConfig struct {
	namespace string
	btlConfig map[string]uint64
}

func testutilNewProviderWithCollectionConfig(
	t *testing.T,
	nsCollBtlConfigs []*nsCollBtlConfig,
	conf *lgr.Config,
) *Provider {
	provider := testutilNewProvider(conf, t, &mock.DeployedChaincodeInfoProvider{})
	mockCCInfoProvider := provider.initializer.DeployedChaincodeInfoProvider.(*mock.DeployedChaincodeInfoProvider)
	collectionConfPkgs := []*peer.CollectionConfigPackage{}

	nsCollMap := map[string]map[string]*peer.StaticCollectionConfig{}
	for _, nsCollBtlConf := range nsCollBtlConfigs {
		collMap := map[string]*peer.StaticCollectionConfig{}
		var collConf []*peer.CollectionConfig
		for collName, btl := range nsCollBtlConf.btlConfig {
			staticConf := &peer.StaticCollectionConfig{Name: collName, BlockToLive: btl}
			collMap[collName] = staticConf
			collectionConf := &peer.CollectionConfig{}
			collectionConf.Payload = &peer.CollectionConfig_StaticCollectionConfig{StaticCollectionConfig: staticConf}
			collConf = append(collConf, collectionConf)
		}
		collectionConfPkgs = append(collectionConfPkgs, &peer.CollectionConfigPackage{Config: collConf})
		nsCollMap[nsCollBtlConf.namespace] = collMap
	}

	mockCCInfoProvider.ChaincodeInfoStub = func(channelName, ccName string, qe lgr.SimpleQueryExecutor) (*lgr.DeployedChaincodeInfo, error) {
		for i, nsCollBtlConf := range nsCollBtlConfigs {
			if ccName == nsCollBtlConf.namespace {
				return &lgr.DeployedChaincodeInfo{
					Name: nsCollBtlConf.namespace, ExplicitCollectionConfigPkg: collectionConfPkgs[i]}, nil
			}
		}
		return nil, nil
	}

	mockCCInfoProvider.AllCollectionsConfigPkgStub = func(channelName, ccName string, qe lgr.SimpleQueryExecutor) (*peer.CollectionConfigPackage, error) {
		for i, nsCollBtlConf := range nsCollBtlConfigs {
			if ccName == nsCollBtlConf.namespace {
				return collectionConfPkgs[i], nil
			}
		}
		return nil, nil

	}

	mockCCInfoProvider.CollectionInfoStub = func(channelName, ccName, collName string, qe lgr.SimpleQueryExecutor) (*peer.StaticCollectionConfig, error) {
		for _, nsCollBtlConf := range nsCollBtlConfigs {
			if ccName == nsCollBtlConf.namespace {
				return nsCollMap[nsCollBtlConf.namespace][collName], nil
			}
		}
		return nil, nil
	}
	return provider
}

func verifyLedgerDoesNotExist(t *testing.T, provider *Provider, ledgerID string) {
	exists, err := provider.idStore.ledgerIDExists(ledgerID)
	require.NoError(t, err)
	require.False(t, exists)

	exists, err = provider.blkStoreProvider.Exists(ledgerID)
	require.NoError(t, err)
	require.False(t, exists)

	db, err := provider.dbProvider.GetDBHandle(ledgerID, nil)
	require.NoError(t, err)
	itr, _, err := db.GetFullScanIterator(func(string) bool { return false })
	require.NoError(t, err)
	k, _, err := itr.Next()
	require.NoError(t, err)
	require.Nil(t, k)
	sp, err := db.GetLatestSavePoint()
	require.NoError(t, err)
	require.Nil(t, sp)

	historydb := provider.historydbProvider.GetDBHandle(ledgerID)
	sp, err = historydb.GetLastSavepoint()
	require.NoError(t, err)
	require.Nil(t, sp)
}

func verifyLedgerIDExists(t *testing.T, provider *Provider, ledgerID string, expectedStatus msgs.Status) {
	exists, err := provider.idStore.ledgerIDExists(ledgerID)
	require.NoError(t, err)
	require.True(t, exists)

	metadata, err := provider.idStore.getLedgerMetadata(ledgerID)
	require.NoError(t, err)
	require.Equal(t, metadata.Status, expectedStatus)
}
