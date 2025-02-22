package clients

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	api "github.com/ethereum/go-ethereum/core/beacon"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/hive/hivesim"

	"github.com/ethereum/hive/simulators/eth2/common/builder"
	"github.com/ethereum/hive/simulators/eth2/common/utils"
	"github.com/holiman/uint256"
	"github.com/protolambda/eth2api"
	"github.com/protolambda/eth2api/client/beaconapi"
	"github.com/protolambda/eth2api/client/debugapi"
	"github.com/protolambda/eth2api/client/nodeapi"
	"github.com/protolambda/zrnt/eth2/beacon/altair"
	"github.com/protolambda/zrnt/eth2/beacon/bellatrix"
	"github.com/protolambda/zrnt/eth2/beacon/capella"
	"github.com/protolambda/zrnt/eth2/beacon/common"
	"github.com/protolambda/zrnt/eth2/beacon/phase0"
	"github.com/protolambda/ztyp/tree"
)

const (
	PortBeaconTCP    = 9000
	PortBeaconUDP    = 9000
	PortBeaconAPI    = 4000
	PortBeaconGRPC   = 4001
	PortMetrics      = 8080
	PortValidatorAPI = 5000
)

var EMPTY_TREE_ROOT = tree.Root{}

type BeaconClient struct {
	T                     *hivesim.T
	HiveClient            *hivesim.Client
	ClientType            string
	OptionsGenerator      func() ([]hivesim.StartOption, error)
	API                   *eth2api.Eth2HttpClient
	genesisTime           common.Timestamp
	spec                  *common.Spec
	index                 int
	genesisValidatorsRoot tree.Root
	Builder               builder.Builder
}

func NewBeaconClient(
	t *hivesim.T,
	beaconDef *hivesim.ClientDefinition,
	optionsGenerator func() ([]hivesim.StartOption, error),
	genesisTime common.Timestamp,
	spec *common.Spec,
	index int,
	genesisValidatorsRoot tree.Root,
) *BeaconClient {
	return &BeaconClient{
		T:                     t,
		ClientType:            beaconDef.Name,
		OptionsGenerator:      optionsGenerator,
		genesisTime:           genesisTime,
		spec:                  spec,
		index:                 index,
		genesisValidatorsRoot: genesisValidatorsRoot,
	}
}

func (bn *BeaconClient) Start(extraOptions ...hivesim.StartOption) error {
	if bn.HiveClient != nil {
		return fmt.Errorf("client already started")
	}
	bn.T.Logf("Starting client %s", bn.ClientType)
	opts, err := bn.OptionsGenerator()
	if err != nil {
		return fmt.Errorf("unable to get start options: %v", err)
	}
	opts = append(opts, extraOptions...)

	if bn.Builder != nil {
		opts = append(opts, hivesim.Params{
			"HIVE_ETH2_BUILDER_ENDPOINT": bn.Builder.Address(),
		})
	}

	bn.HiveClient = bn.T.StartClient(bn.ClientType, opts...)
	bn.API = &eth2api.Eth2HttpClient{
		Addr:  fmt.Sprintf("http://%s:%d", bn.HiveClient.IP, PortBeaconAPI),
		Cli:   &http.Client{},
		Codec: eth2api.JSONCodec{},
	}
	bn.T.Logf(
		"Started client %s, container: %s",
		bn.ClientType,
		bn.HiveClient.Container,
	)
	return nil
}

func (bn *BeaconClient) Shutdown() error {
	if err := bn.T.Sim.StopClient(bn.T.SuiteID, bn.T.TestID, bn.HiveClient.Container); err != nil {
		return err
	}
	bn.HiveClient = nil
	return nil
}

func (bn *BeaconClient) IsRunning() bool {
	return bn.HiveClient != nil
}

func (bn *BeaconClient) ENR(parentCtx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parentCtx, time.Second*10)
	defer cancel()
	var out eth2api.NetworkIdentity
	if err := nodeapi.Identity(ctx, bn.API, &out); err != nil {
		return "", err
	}
	fmt.Printf("p2p addrs: %v\n", out.P2PAddresses)
	fmt.Printf("peer id: %s\n", out.PeerID)
	fmt.Printf("enr: %s\n", out.ENR)
	return out.ENR, nil
}

func (bn *BeaconClient) P2PAddr(parentCtx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parentCtx, time.Second*10)
	defer cancel()
	var out eth2api.NetworkIdentity
	if err := nodeapi.Identity(ctx, bn.API, &out); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"/ip4/%s/tcp/%d/p2p/%s",
		bn.HiveClient.IP.String(),
		PortBeaconTCP,
		out.PeerID,
	), nil
}

func (bn *BeaconClient) EnodeURL() (string, error) {
	return "", errors.New(
		"beacon node does not have an discv4 Enode URL, use ENR or multi-address instead",
	)
}

func (bn *BeaconClient) ClientName() string {
	name := bn.ClientType
	if len(name) > 3 && name[len(name)-3:] == "-bn" {
		name = name[:len(name)-3]
	}
	return name
}

// Beacon API wrappers
type VersionedSignedBeaconBlock struct {
	*eth2api.VersionedSignedBeaconBlock
}

func (versionedBlock *VersionedSignedBeaconBlock) ContainsExecutionPayload() bool {
	return versionedBlock.Version == "bellatrix" ||
		versionedBlock.Version == "capella"
}

func (versionedBlock *VersionedSignedBeaconBlock) ExecutionPayload() (api.ExecutableData, error) {
	result := api.ExecutableData{}
	switch v := versionedBlock.Data.(type) {
	case *bellatrix.SignedBeaconBlock:
		execPayload := v.Message.Body.ExecutionPayload
		copy(result.ParentHash[:], execPayload.ParentHash[:])
		copy(result.FeeRecipient[:], execPayload.FeeRecipient[:])
		copy(result.StateRoot[:], execPayload.StateRoot[:])
		copy(result.ReceiptsRoot[:], execPayload.ReceiptsRoot[:])
		copy(result.LogsBloom[:], execPayload.LogsBloom[:])
		copy(result.Random[:], execPayload.PrevRandao[:])
		result.Number = uint64(execPayload.BlockNumber)
		result.GasLimit = uint64(execPayload.GasLimit)
		result.GasUsed = uint64(execPayload.GasUsed)
		result.Timestamp = uint64(execPayload.Timestamp)
		copy(result.ExtraData[:], execPayload.ExtraData[:])
		result.BaseFeePerGas = (*uint256.Int)(&execPayload.BaseFeePerGas).ToBig()
		copy(result.BlockHash[:], execPayload.BlockHash[:])
		result.Transactions = make([][]byte, 0)
		for _, t := range execPayload.Transactions {
			result.Transactions = append(result.Transactions, t)
		}
	case *capella.SignedBeaconBlock:
		execPayload := v.Message.Body.ExecutionPayload
		copy(result.ParentHash[:], execPayload.ParentHash[:])
		copy(result.FeeRecipient[:], execPayload.FeeRecipient[:])
		copy(result.StateRoot[:], execPayload.StateRoot[:])
		copy(result.ReceiptsRoot[:], execPayload.ReceiptsRoot[:])
		copy(result.LogsBloom[:], execPayload.LogsBloom[:])
		copy(result.Random[:], execPayload.PrevRandao[:])
		result.Number = uint64(execPayload.BlockNumber)
		result.GasLimit = uint64(execPayload.GasLimit)
		result.GasUsed = uint64(execPayload.GasUsed)
		result.Timestamp = uint64(execPayload.Timestamp)
		copy(result.ExtraData[:], execPayload.ExtraData[:])
		result.BaseFeePerGas = (*uint256.Int)(&execPayload.BaseFeePerGas).ToBig()
		copy(result.BlockHash[:], execPayload.BlockHash[:])
		result.Transactions = make([][]byte, 0)
		for _, t := range execPayload.Transactions {
			result.Transactions = append(result.Transactions, t)
		}
		result.Withdrawals = make([]*types.Withdrawal, 0)
		for _, w := range execPayload.Withdrawals {
			withdrawal := new(types.Withdrawal)
			withdrawal.Index = uint64(w.Index)
			withdrawal.Validator = uint64(w.ValidatorIndex)
			copy(withdrawal.Address[:], w.Address[:])
			withdrawal.Amount = uint64(w.Amount)
			result.Withdrawals = append(result.Withdrawals, withdrawal)
		}
	default:
		return result, fmt.Errorf(
			"beacon block version can't contain execution payload",
		)
	}
	return result, nil
}

func (versionedBlock *VersionedSignedBeaconBlock) Withdrawals() (common.Withdrawals, error) {
	switch v := versionedBlock.Data.(type) {
	case *capella.SignedBeaconBlock:
		return v.Message.Body.ExecutionPayload.Withdrawals, nil
	}
	return nil, nil
}

func (b *VersionedSignedBeaconBlock) StateRoot() tree.Root {
	switch v := b.Data.(type) {
	case *phase0.SignedBeaconBlock:
		return v.Message.StateRoot
	case *altair.SignedBeaconBlock:
		return v.Message.StateRoot
	case *bellatrix.SignedBeaconBlock:
		return v.Message.StateRoot
	case *capella.SignedBeaconBlock:
		return v.Message.StateRoot
	}
	panic("badly formatted beacon block")
}

func (b *VersionedSignedBeaconBlock) ParentRoot() tree.Root {
	switch v := b.Data.(type) {
	case *phase0.SignedBeaconBlock:
		return v.Message.ParentRoot
	case *altair.SignedBeaconBlock:
		return v.Message.ParentRoot
	case *bellatrix.SignedBeaconBlock:
		return v.Message.ParentRoot
	case *capella.SignedBeaconBlock:
		return v.Message.ParentRoot
	}
	panic("badly formatted beacon block")
}

func (b *VersionedSignedBeaconBlock) Slot() common.Slot {
	switch v := b.Data.(type) {
	case *phase0.SignedBeaconBlock:
		return v.Message.Slot
	case *altair.SignedBeaconBlock:
		return v.Message.Slot
	case *bellatrix.SignedBeaconBlock:
		return v.Message.Slot
	case *capella.SignedBeaconBlock:
		return v.Message.Slot
	}
	panic("badly formatted beacon block")
}

func (b *VersionedSignedBeaconBlock) ProposerIndex() common.ValidatorIndex {
	switch v := b.Data.(type) {
	case *phase0.SignedBeaconBlock:
		return v.Message.ProposerIndex
	case *altair.SignedBeaconBlock:
		return v.Message.ProposerIndex
	case *bellatrix.SignedBeaconBlock:
		return v.Message.ProposerIndex
	case *capella.SignedBeaconBlock:
		return v.Message.ProposerIndex
	}
	panic("badly formatted beacon block")
}

func (bn *BeaconClient) BlockV2Root(
	parentCtx context.Context,
	blockId eth2api.BlockId,
) (tree.Root, error) {
	var (
		root   tree.Root
		exists bool
		err    error
	)
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	root, exists, err = beaconapi.BlockRoot(ctx, bn.API, blockId)
	if !exists {
		return root, fmt.Errorf(
			"endpoint not found on beacon client",
		)
	}
	return root, err
}

func (bn *BeaconClient) BlockV2(
	parentCtx context.Context,
	blockId eth2api.BlockId,
) (*VersionedSignedBeaconBlock, error) {
	var (
		versionedBlock = new(eth2api.VersionedSignedBeaconBlock)
		exists         bool
		err            error
	)
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	exists, err = beaconapi.BlockV2(ctx, bn.API, blockId, versionedBlock)
	if !exists {
		return nil, fmt.Errorf("endpoint not found on beacon client")
	}
	return &VersionedSignedBeaconBlock{
		VersionedSignedBeaconBlock: versionedBlock,
	}, err
}

type BlockV2OptimisticResponse struct {
	Version             string `json:"version"`
	ExecutionOptimistic bool   `json:"execution_optimistic"`
}

func (bn *BeaconClient) BlockIsOptimistic(
	parentCtx context.Context,
	blockId eth2api.BlockId,
) (bool, error) {
	var (
		blockOptResp = new(BlockV2OptimisticResponse)
		exists       bool
		err          error
	)
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	exists, err = eth2api.SimpleRequest(
		ctx,
		bn.API,
		eth2api.FmtGET("/eth/v2/beacon/blocks/%s", blockId.BlockId()),
		blockOptResp,
	)
	if !exists {
		return false, fmt.Errorf("endpoint not found on beacon client")
	}
	return blockOptResp.ExecutionOptimistic, err
}

func (bn *BeaconClient) BlockHeader(
	parentCtx context.Context,
	blockId eth2api.BlockId,
) (*eth2api.BeaconBlockHeaderAndInfo, error) {
	var (
		headInfo = new(eth2api.BeaconBlockHeaderAndInfo)
		exists   bool
		err      error
	)
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	exists, err = beaconapi.BlockHeader(ctx, bn.API, blockId, headInfo)
	if !exists {
		return nil, fmt.Errorf("endpoint not found on beacon client")
	}
	return headInfo, err
}

func (bn *BeaconClient) StateValidator(
	parentCtx context.Context,
	stateId eth2api.StateId,
	validatorId eth2api.ValidatorId,
) (*eth2api.ValidatorResponse, error) {
	var (
		stateValidatorResponse = new(eth2api.ValidatorResponse)
		exists                 bool
		err                    error
	)
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	exists, err = beaconapi.StateValidator(
		ctx,
		bn.API,
		stateId,
		validatorId,
		stateValidatorResponse,
	)
	if !exists {
		return nil, fmt.Errorf("endpoint not found on beacon client")
	}
	return stateValidatorResponse, err
}

func (bn *BeaconClient) StateFinalityCheckpoints(
	parentCtx context.Context,
	stateId eth2api.StateId,
) (*eth2api.FinalityCheckpoints, error) {
	var (
		finalityCheckpointsResponse = new(eth2api.FinalityCheckpoints)
		exists                      bool
		err                         error
	)
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	exists, err = beaconapi.FinalityCheckpoints(
		ctx,
		bn.API,
		stateId,
		finalityCheckpointsResponse,
	)
	if !exists {
		return nil, fmt.Errorf("endpoint not found on beacon client")
	}
	return finalityCheckpointsResponse, err
}

func (bn *BeaconClient) BlockFinalityCheckpoints(
	parentCtx context.Context,
	blockId eth2api.BlockId,
) (*eth2api.FinalityCheckpoints, error) {
	var (
		headInfo                    *eth2api.BeaconBlockHeaderAndInfo
		finalityCheckpointsResponse *eth2api.FinalityCheckpoints
		err                         error
	)
	headInfo, err = bn.BlockHeader(parentCtx, blockId)
	if err != nil {
		return nil, err
	}
	finalityCheckpointsResponse, err = bn.StateFinalityCheckpoints(
		parentCtx,
		eth2api.StateIdRoot(headInfo.Header.Message.StateRoot),
	)
	if err != nil {
		// Try again using slot number
		return bn.StateFinalityCheckpoints(
			parentCtx,
			eth2api.StateIdSlot(headInfo.Header.Message.Slot),
		)
	}
	return finalityCheckpointsResponse, err
}

type VersionedBeaconStateResponse struct {
	*eth2api.VersionedBeaconState
	spec *common.Spec
}

func (vbs *VersionedBeaconStateResponse) Root() tree.Root {
	switch state := vbs.Data.(type) {
	case *phase0.BeaconState:
		return state.HashTreeRoot(vbs.spec, tree.GetHashFn())
	case *altair.BeaconState:
		return state.HashTreeRoot(vbs.spec, tree.GetHashFn())
	case *bellatrix.BeaconState:
		return state.HashTreeRoot(vbs.spec, tree.GetHashFn())
	case *capella.BeaconState:
		return state.HashTreeRoot(vbs.spec, tree.GetHashFn())
	}
	panic("badly formatted beacon state")
}

func (vbs *VersionedBeaconStateResponse) CurrentVersion() common.Version {
	switch state := vbs.Data.(type) {
	case *phase0.BeaconState:
		return state.Fork.CurrentVersion
	case *altair.BeaconState:
		return state.Fork.CurrentVersion
	case *bellatrix.BeaconState:
		return state.Fork.CurrentVersion
	case *capella.BeaconState:
		return state.Fork.CurrentVersion
	}
	panic("badly formatted beacon state")
}

func (vbs *VersionedBeaconStateResponse) PreviousVersion() common.Version {
	switch state := vbs.Data.(type) {
	case *phase0.BeaconState:
		return state.Fork.PreviousVersion
	case *altair.BeaconState:
		return state.Fork.PreviousVersion
	case *bellatrix.BeaconState:
		return state.Fork.PreviousVersion
	case *capella.BeaconState:
		return state.Fork.PreviousVersion
	}
	panic("badly formatted beacon state")
}

func (vbs *VersionedBeaconStateResponse) CurrentEpochParticipation() altair.ParticipationRegistry {
	switch state := vbs.Data.(type) {
	case *altair.BeaconState:
		return state.CurrentEpochParticipation
	case *bellatrix.BeaconState:
		return state.CurrentEpochParticipation
	case *capella.BeaconState:
		return state.CurrentEpochParticipation
	}
	return nil
}

func (vbs *VersionedBeaconStateResponse) Balances() phase0.Balances {
	switch state := vbs.Data.(type) {
	case *phase0.BeaconState:
		return state.Balances
	case *altair.BeaconState:
		return state.Balances
	case *bellatrix.BeaconState:
		return state.Balances
	case *capella.BeaconState:
		return state.Balances
	}
	panic("badly formatted beacon state")
}

func (vbs *VersionedBeaconStateResponse) Balance(
	id common.ValidatorIndex,
) common.Gwei {
	balances := vbs.Balances()
	if int(id) >= len(balances) {
		panic("invalid validator requested")
	}
	return balances[id]
}

func (vbs *VersionedBeaconStateResponse) Validators() phase0.ValidatorRegistry {
	switch state := vbs.Data.(type) {
	case *phase0.BeaconState:
		return state.Validators
	case *altair.BeaconState:
		return state.Validators
	case *bellatrix.BeaconState:
		return state.Validators
	case *capella.BeaconState:
		return state.Validators
	}
	panic("badly formatted beacon state")
}

func (vbs *VersionedBeaconStateResponse) RandaoMixes() phase0.RandaoMixes {
	switch state := vbs.Data.(type) {
	case *phase0.BeaconState:
		return state.RandaoMixes
	case *altair.BeaconState:
		return state.RandaoMixes
	case *bellatrix.BeaconState:
		return state.RandaoMixes
	case *capella.BeaconState:
		return state.RandaoMixes
	}
	panic("badly formatted beacon state")
}

func (vbs *VersionedBeaconStateResponse) StateSlot() common.Slot {
	switch state := vbs.Data.(type) {
	case *phase0.BeaconState:
		return state.Slot
	case *altair.BeaconState:
		return state.Slot
	case *bellatrix.BeaconState:
		return state.Slot
	case *capella.BeaconState:
		return state.Slot
	}
	panic("badly formatted beacon state")
}

func (vbs *VersionedBeaconStateResponse) LatestExecutionPayloadHeaderHash() tree.Root {
	switch state := vbs.Data.(type) {
	case *phase0.BeaconState:
		return tree.Root{}
	case *altair.BeaconState:
		return tree.Root{}
	case *bellatrix.BeaconState:
		return state.LatestExecutionPayloadHeader.BlockHash
	case *capella.BeaconState:
		return state.LatestExecutionPayloadHeader.BlockHash
	}
	panic("badly formatted beacon state")
}

func (vbs *VersionedBeaconStateResponse) NextWithdrawalIndex() (common.WithdrawalIndex, error) {
	var wIndex common.WithdrawalIndex
	switch state := vbs.Data.(type) {
	case *capella.BeaconState:
		wIndex = state.NextWithdrawalIndex
	}
	return wIndex, nil
}

func (vbs *VersionedBeaconStateResponse) NextWithdrawalValidatorIndex() (common.ValidatorIndex, error) {
	var wIndex common.ValidatorIndex
	switch state := vbs.Data.(type) {
	case *capella.BeaconState:
		wIndex = state.NextWithdrawalValidatorIndex
	}
	return wIndex, nil
}

func (vbs *VersionedBeaconStateResponse) NextWithdrawals(
	slot common.Slot,
) (common.Withdrawals, error) {
	var (
		withdrawalIndex common.WithdrawalIndex
		validatorIndex  common.ValidatorIndex
		validators      phase0.ValidatorRegistry
		balances        phase0.Balances
		epoch           = vbs.spec.SlotToEpoch(slot)
	)
	switch state := vbs.Data.(type) {
	case *bellatrix.BeaconState:
		// withdrawalIndex and validatorIndex start at zero
		validators = state.Validators
		balances = state.Balances
	case *capella.BeaconState:
		withdrawalIndex = state.NextWithdrawalIndex
		validatorIndex = state.NextWithdrawalValidatorIndex
		validators = state.Validators
		balances = state.Balances
	default:
		return nil, fmt.Errorf("badly formatted beacon state")
	}
	validatorCount := uint64(len(validators))
	withdrawals := make(common.Withdrawals, 0)

	i := uint64(0)
	for {
		if validatorIndex >= common.ValidatorIndex(validatorCount) ||
			validatorIndex >= common.ValidatorIndex(len(balances)) {
			return nil, fmt.Errorf("invalid validator index")
		}
		validator := validators[validatorIndex]
		if validator == nil {
			return nil, fmt.Errorf("invalid validator")
		}
		balance := balances[validatorIndex]
		if i >= validatorCount ||
			i >= uint64(vbs.spec.MAX_VALIDATORS_PER_WITHDRAWALS_SWEEP) {
			break
		}
		if IsFullyWithdrawableValidator(validator, balance, epoch) {
			withdrawals = append(withdrawals, common.Withdrawal{
				Index:          withdrawalIndex,
				ValidatorIndex: validatorIndex,
				Address:        Eth1WithdrawalCredential(validator),
				Amount:         balance,
			})
			withdrawalIndex += 1
		} else if IsPartiallyWithdrawableValidator(vbs.spec, validator, balance, epoch) {
			withdrawals = append(withdrawals, common.Withdrawal{
				Index:          withdrawalIndex,
				ValidatorIndex: validatorIndex,
				Address:        Eth1WithdrawalCredential(validator),
				Amount:         balance - vbs.spec.MAX_EFFECTIVE_BALANCE,
			})
			withdrawalIndex += 1
		}
		if len(withdrawals) == int(vbs.spec.MAX_WITHDRAWALS_PER_PAYLOAD) {
			break
		}
		validatorIndex = common.ValidatorIndex(
			uint64(validatorIndex+1) % validatorCount,
		)
		i += 1
	}
	return withdrawals, nil
}

func Eth1WithdrawalCredential(validator *phase0.Validator) common.Eth1Address {
	var address common.Eth1Address
	copy(address[:], validator.WithdrawalCredentials[12:])
	return address
}

func IsFullyWithdrawableValidator(
	validator *phase0.Validator,
	balance common.Gwei,
	epoch common.Epoch,
) bool {
	return HasEth1WithdrawalCredential(validator) &&
		validator.WithdrawableEpoch <= epoch &&
		balance > 0
}

func IsPartiallyWithdrawableValidator(
	spec *common.Spec,
	validator *phase0.Validator,
	balance common.Gwei,
	epoch common.Epoch,
) bool {
	effectiveBalance := validator.EffectiveBalance
	hasMaxEffectiveBalance := effectiveBalance == spec.MAX_EFFECTIVE_BALANCE
	hasExcessBalance := balance > spec.MAX_EFFECTIVE_BALANCE
	return HasEth1WithdrawalCredential(validator) && hasMaxEffectiveBalance &&
		hasExcessBalance
}

func HasEth1WithdrawalCredential(validator *phase0.Validator) bool {
	return bytes.Equal(
		validator.WithdrawalCredentials[:1],
		[]byte{common.ETH1_ADDRESS_WITHDRAWAL_PREFIX},
	)
}

func (bn *BeaconClient) BeaconStateV2(
	parentCtx context.Context,
	stateId eth2api.StateId,
) (*VersionedBeaconStateResponse, error) {
	var (
		versionedBeaconStateResponse = new(eth2api.VersionedBeaconState)
		exists                       bool
		err                          error
	)
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	exists, err = debugapi.BeaconStateV2(
		ctx,
		bn.API,
		stateId,
		versionedBeaconStateResponse,
	)
	if !exists {
		return nil, fmt.Errorf("endpoint not found on beacon client")
	}
	return &VersionedBeaconStateResponse{
		VersionedBeaconState: versionedBeaconStateResponse,
		spec:                 bn.spec,
	}, err
}

func (bn *BeaconClient) BeaconStateV2ByBlock(
	parentCtx context.Context,
	blockId eth2api.BlockId,
) (*VersionedBeaconStateResponse, error) {
	var (
		headInfo *eth2api.BeaconBlockHeaderAndInfo
		err      error
	)
	headInfo, err = bn.BlockHeader(parentCtx, blockId)
	if err != nil {
		return nil, err
	}
	return bn.BeaconStateV2(
		parentCtx,
		eth2api.StateIdRoot(headInfo.Header.Message.StateRoot),
	)
}

func (bn *BeaconClient) StateValidators(
	parentCtx context.Context,
	stateId eth2api.StateId,
	validatorIds []eth2api.ValidatorId,
	statusFilter []eth2api.ValidatorStatus,
) ([]eth2api.ValidatorResponse, error) {
	var (
		stateValidatorResponse = make(
			[]eth2api.ValidatorResponse,
			0,
		)
		exists bool
		err    error
	)
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	exists, err = beaconapi.StateValidators(
		ctx,
		bn.API,
		stateId,
		validatorIds,
		statusFilter,
		&stateValidatorResponse,
	)
	if !exists {
		return nil, fmt.Errorf("endpoint not found on beacon client")
	}
	return stateValidatorResponse, err
}

func (bn *BeaconClient) StateValidatorBalances(
	parentCtx context.Context,
	stateId eth2api.StateId,
	validatorIds []eth2api.ValidatorId,
) ([]eth2api.ValidatorBalanceResponse, error) {
	var (
		stateValidatorBalanceResponse = make(
			[]eth2api.ValidatorBalanceResponse,
			0,
		)
		exists bool
		err    error
	)
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	exists, err = beaconapi.StateValidatorBalances(
		ctx,
		bn.API,
		stateId,
		validatorIds,
		&stateValidatorBalanceResponse,
	)
	if !exists {
		return nil, fmt.Errorf("endpoint not found on beacon client")
	}
	return stateValidatorBalanceResponse, err
}

func (bn *BeaconClient) ComputeDomain(
	ctx context.Context,
	typ common.BLSDomainType,
	version *common.Version,
) (common.BLSDomain, error) {
	if version != nil {
		return common.ComputeDomain(
			typ,
			*version,
			bn.genesisValidatorsRoot,
		), nil
	}
	// We need to request for head state to know current active version
	state, err := bn.BeaconStateV2ByBlock(ctx, eth2api.BlockHead)
	if err != nil {
		return common.BLSDomain{}, err
	}
	return common.ComputeDomain(
		typ,
		state.CurrentVersion(),
		bn.genesisValidatorsRoot,
	), nil
}

func (bn *BeaconClient) SubmitPoolBLSToExecutionChange(
	parentCtx context.Context,
	l common.SignedBLSToExecutionChanges,
) error {
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	return beaconapi.SubmitBLSToExecutionChanges(ctx, bn.API, l)
}

func (bn *BeaconClient) SubmitVoluntaryExit(
	parentCtx context.Context,
	exit *phase0.SignedVoluntaryExit,
) error {
	ctx, cancel := utils.ContextTimeoutRPC(parentCtx)
	defer cancel()
	return beaconapi.SubmitVoluntaryExit(ctx, bn.API, exit)
}

func (b *BeaconClient) WaitForExecutionPayload(
	ctx context.Context,
) (ethcommon.Hash, error) {
	fmt.Printf(
		"Waiting for execution payload on beacon %d (%s)\n",
		b.index,
		b.ClientName(),
	)
	slotDuration := time.Duration(b.spec.SECONDS_PER_SLOT) * time.Second
	timer := time.NewTicker(slotDuration)

	for {
		select {
		case <-ctx.Done():
			return ethcommon.Hash{}, ctx.Err()
		case <-timer.C:
			realTimeSlot := b.spec.TimeToSlot(
				common.Timestamp(time.Now().Unix()),
				b.genesisTime,
			)
			var (
				headInfo  *eth2api.BeaconBlockHeaderAndInfo
				err       error
				execution ethcommon.Hash
			)
			if headInfo, err = b.BlockHeader(ctx, eth2api.BlockHead); err != nil {
				return ethcommon.Hash{}, err
			}
			if !headInfo.Canonical {
				continue
			}

			if versionedBlock, err := b.BlockV2(ctx, eth2api.BlockIdRoot(headInfo.Root)); err != nil {
				continue
			} else if executionPayload, err := versionedBlock.ExecutionPayload(); err == nil {
				copy(
					execution[:],
					executionPayload.BlockHash[:],
				)
			}
			zero := ethcommon.Hash{}
			fmt.Printf(
				"WaitForExecutionPayload: beacon %d (%s): slot=%d, realTimeSlot=%d, head=%s, exec=%s\n",
				b.index,
				b.ClientName(),
				headInfo.Header.Message.Slot,
				realTimeSlot,
				utils.Shorten(headInfo.Root.String()),
				utils.Shorten(execution.Hex()),
			)
			if !bytes.Equal(execution[:], zero[:]) {
				return execution, nil
			}
		}
	}
}

func (b *BeaconClient) WaitForOptimisticState(
	ctx context.Context,
	blockID eth2api.BlockId,
	optimistic bool,
) (*eth2api.BeaconBlockHeaderAndInfo, error) {
	fmt.Printf("Waiting for optimistic sync on beacon %d (%s)\n",
		b.index,
		b.ClientName(),
	)
	slotDuration := time.Duration(b.spec.SECONDS_PER_SLOT) * time.Second
	timer := time.NewTicker(slotDuration)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			var headOptStatus BlockV2OptimisticResponse
			if exists, err := eth2api.SimpleRequest(ctx, b.API, eth2api.FmtGET("/eth/v2/beacon/blocks/%s", blockID.BlockId()), &headOptStatus); err != nil {
				// Block still not synced
				continue
			} else if !exists {
				// Block still not synced
				continue
			}
			if headOptStatus.ExecutionOptimistic != optimistic {
				continue
			}
			// Return the block
			var blockInfo eth2api.BeaconBlockHeaderAndInfo
			if exists, err := beaconapi.BlockHeader(ctx, b.API, blockID, &blockInfo); err != nil {
				return nil, fmt.Errorf(
					"WaitForExecutionPayload: failed to poll block: %v",
					err,
				)
			} else if !exists {
				return nil, fmt.Errorf("WaitForExecutionPayload: failed to poll block: !exists")
			}
			return &blockInfo, nil
		}
	}
}

func (bn *BeaconClient) GetLatestExecutionBeaconBlock(
	parentCtx context.Context,
) (*VersionedSignedBeaconBlock, error) {
	headInfo, err := bn.BlockHeader(parentCtx, eth2api.BlockHead)
	if err != nil {
		return nil, fmt.Errorf("failed to poll head: %v", err)
	}
	for slot := headInfo.Header.Message.Slot; slot > 0; slot-- {
		versionedBlock, err := bn.BlockV2(parentCtx, eth2api.BlockIdSlot(slot))
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve block: %v", err)
		}
		if executionPayload, err := versionedBlock.ExecutionPayload(); err == nil {
			if !bytes.Equal(
				executionPayload.BlockHash[:],
				EMPTY_TREE_ROOT[:],
			) {
				return versionedBlock, nil
			}
		}
	}
	return nil, nil
}

func (bn *BeaconClient) GetFirstExecutionBeaconBlock(
	parentCtx context.Context,
) (*VersionedSignedBeaconBlock, error) {
	lastSlot := bn.spec.TimeToSlot(
		common.Timestamp(time.Now().Unix()),
		bn.genesisTime,
	)
	for slot := common.Slot(0); slot <= lastSlot; slot++ {
		versionedBlock, err := bn.BlockV2(parentCtx, eth2api.BlockIdSlot(slot))
		if err != nil {
			continue
		}
		if executionPayload, err := versionedBlock.ExecutionPayload(); err == nil {
			if !bytes.Equal(
				executionPayload.BlockHash[:],
				EMPTY_TREE_ROOT[:],
			) {
				return versionedBlock, nil
			}
		}
	}
	return nil, nil
}

func (bn *BeaconClient) GetBeaconBlockByExecutionHash(
	parentCtx context.Context,
	hash ethcommon.Hash,
) (*VersionedSignedBeaconBlock, error) {
	headInfo, err := bn.BlockHeader(parentCtx, eth2api.BlockHead)
	if err != nil {
		return nil, fmt.Errorf("failed to poll head: %v", err)
	}
	for slot := int(headInfo.Header.Message.Slot); slot > 0; slot -= 1 {
		versionedBlock, err := bn.BlockV2(parentCtx, eth2api.BlockIdSlot(slot))
		if err != nil {
			continue
		}
		if executionPayload, err := versionedBlock.ExecutionPayload(); err == nil {
			if !bytes.Equal(executionPayload.BlockHash[:], hash[:]) {
				return versionedBlock, nil
			}
		}
	}
	return nil, nil
}

func (bn *BeaconClient) GetFilledSlotsCountPerEpoch(
	parentCtx context.Context,
) (map[common.Epoch]uint64, error) {
	headInfo, err := bn.BlockHeader(parentCtx, eth2api.BlockHead)
	epochMap := make(map[common.Epoch]uint64)
	for {
		if err != nil {
			return nil, fmt.Errorf("failed to poll head: %v", err)
		}
		epoch := common.Epoch(
			headInfo.Header.Message.Slot / bn.spec.SLOTS_PER_EPOCH,
		)
		if prev, ok := epochMap[epoch]; ok {
			epochMap[epoch] = prev + 1
		} else {
			epochMap[epoch] = 1
		}
		if bytes.Equal(
			headInfo.Header.Message.ParentRoot[:],
			EMPTY_TREE_ROOT[:],
		) {
			break
		}
		headInfo, err = bn.BlockHeader(
			parentCtx,
			eth2api.BlockIdRoot(headInfo.Header.Message.ParentRoot),
		)
	}

	return epochMap, nil
}

type BeaconClients []*BeaconClient

// Return subset of clients that are currently running
func (all BeaconClients) Running() BeaconClients {
	res := make(BeaconClients, 0)
	for _, bc := range all {
		if bc.IsRunning() {
			res = append(res, bc)
		}
	}
	return res
}

// Returns comma-separated ENRs of all running beacon nodes
func (beacons BeaconClients) ENRs(parentCtx context.Context) (string, error) {
	if len(beacons) == 0 {
		return "", nil
	}
	enrs := make([]string, 0)
	for _, bn := range beacons {
		if bn.IsRunning() {
			enr, err := bn.ENR(parentCtx)
			if err != nil {
				return "", err
			}
			enrs = append(enrs, enr)
		}
	}
	return strings.Join(enrs, ","), nil
}

// Returns comma-separated P2PAddr of all running beacon nodes
func (beacons BeaconClients) P2PAddrs(
	parentCtx context.Context,
) (string, error) {
	if len(beacons) == 0 {
		return "", nil
	}
	staticPeers := make([]string, 0)
	for _, bn := range beacons {
		if bn.IsRunning() {
			p2p, err := bn.P2PAddr(parentCtx)
			if err != nil {
				return "", err
			}
			staticPeers = append(staticPeers, p2p)
		}
	}
	return strings.Join(staticPeers, ","), nil
}

func (b BeaconClients) GetBeaconBlockByExecutionHash(
	parentCtx context.Context,
	hash ethcommon.Hash,
) (*VersionedSignedBeaconBlock, error) {
	for _, bn := range b {
		block, err := bn.GetBeaconBlockByExecutionHash(parentCtx, hash)
		if err != nil || block != nil {
			return block, err
		}
	}
	return nil, nil
}

func (runningBeacons BeaconClients) PrintStatus(
	ctx context.Context,
	l utils.Logging,
) {
	for i, b := range runningBeacons {
		var (
			slot      common.Slot
			version   string
			head      string
			justified string
			finalized string
			execution = "0x0000..0000"
		)

		if headInfo, err := b.BlockHeader(ctx, eth2api.BlockHead); err == nil {
			slot = headInfo.Header.Message.Slot
			head = utils.Shorten(headInfo.Root.String())
		}
		checkpoints, err := b.BlockFinalityCheckpoints(
			ctx,
			eth2api.BlockHead,
		)
		if err == nil {
			justified = utils.Shorten(
				checkpoints.CurrentJustified.String(),
			)
			finalized = utils.Shorten(checkpoints.Finalized.String())
		}
		if versionedBlock, err := b.BlockV2(ctx, eth2api.BlockHead); err == nil {
			version = versionedBlock.Version
			if executionPayload, err := versionedBlock.ExecutionPayload(); err == nil {
				execution = utils.Shorten(
					executionPayload.BlockHash.String(),
				)
			}
		}

		l.Logf(
			"beacon %d (%s): fork=%s, slot=%d, head=%s, exec_payload=%s, justified=%s, finalized=%s",
			i,
			b.ClientName(),
			version,
			slot,
			head,
			execution,
			justified,
			finalized,
		)
	}
}

func (all BeaconClients) SubmitPoolBLSToExecutionChange(
	parentCtx context.Context,
	l common.SignedBLSToExecutionChanges,
) error {
	for _, b := range all {
		if err := b.SubmitPoolBLSToExecutionChange(parentCtx, l); err != nil {
			return err
		}
	}
	return nil
}
