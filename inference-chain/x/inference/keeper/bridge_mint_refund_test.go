package keeper_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/testutil"
	keepertest "github.com/productscience/inference/testutil/keeper"
	blstypes "github.com/productscience/inference/x/bls/types"
	"github.com/productscience/inference/x/inference/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestProcessBridgeMintRefunds_RefundsFailedRequest(t *testing.T) {
	k, ctx, mocks := keepertest.InferenceKeeperReturningMocks(t)

	ctrl := gomock.NewController(t)
	blsMock := keepertest.NewMockBlsKeeper(ctrl)
	k.BlsKeeper = blsMock

	requestID := bytes.Repeat([]byte{0x11}, 32)
	requestIDHex := hex.EncodeToString(requestID)
	pending := types.MsgRequestBridgeMint{
		Creator:            testutil.Creator,
		Amount:             "1000",
		DestinationAddress: "0xabc",
		ChainId:            "ethereum",
	}
	require.NoError(t, k.BridgeMintRefundsMap.Set(ctx, requestIDHex, pending))

	blsMock.EXPECT().
		GetSigningStatus(ctx, requestID).
		Return(&blstypes.ThresholdSigningRequest{Status: blstypes.ThresholdSigningStatus_THRESHOLD_SIGNING_STATUS_FAILED}, nil)

	creatorAddr, err := sdk.AccAddressFromBech32(testutil.Creator)
	require.NoError(t, err)
	refundCoins := sdk.NewCoins(sdk.NewCoin(types.BaseCoin, math.NewInt(1000)))
	mocks.BankKeeper.EXPECT().
		SendCoinsFromModuleToAccount(ctx, types.BridgeEscrowAccName, creatorAddr, refundCoins, "bridge_release").
		Return(nil)

	require.NoError(t, k.ProcessBridgeMintRefunds(ctx))

	_, err = k.BridgeMintRefundsMap.Get(ctx, requestIDHex)
	require.ErrorIs(t, err, collections.ErrNotFound)
}

func TestProcessBridgeMintRefunds_CleansCompletedRequestWithoutRefund(t *testing.T) {
	k, ctx, _ := keepertest.InferenceKeeperReturningMocks(t)

	ctrl := gomock.NewController(t)
	blsMock := keepertest.NewMockBlsKeeper(ctrl)
	k.BlsKeeper = blsMock

	requestID := bytes.Repeat([]byte{0x22}, 32)
	requestIDHex := hex.EncodeToString(requestID)
	pending := types.MsgRequestBridgeMint{
		Creator:            testutil.Creator,
		Amount:             "1000",
		DestinationAddress: "0xabc",
		ChainId:            "ethereum",
	}
	require.NoError(t, k.BridgeMintRefundsMap.Set(ctx, requestIDHex, pending))

	blsMock.EXPECT().
		GetSigningStatus(ctx, requestID).
		Return(&blstypes.ThresholdSigningRequest{Status: blstypes.ThresholdSigningStatus_THRESHOLD_SIGNING_STATUS_COMPLETED}, nil)

	require.NoError(t, k.ProcessBridgeMintRefunds(ctx))

	_, err := k.BridgeMintRefundsMap.Get(ctx, requestIDHex)
	require.ErrorIs(t, err, collections.ErrNotFound)
}

func TestProcessBridgeMintRefunds_LeavesCollectingRequestPending(t *testing.T) {
	k, ctx, _ := keepertest.InferenceKeeperReturningMocks(t)

	ctrl := gomock.NewController(t)
	blsMock := keepertest.NewMockBlsKeeper(ctrl)
	k.BlsKeeper = blsMock

	requestID := bytes.Repeat([]byte{0x33}, 32)
	requestIDHex := hex.EncodeToString(requestID)
	pending := types.MsgRequestBridgeMint{
		Creator:            testutil.Creator,
		Amount:             "1000",
		DestinationAddress: "0xabc",
		ChainId:            "ethereum",
	}
	require.NoError(t, k.BridgeMintRefundsMap.Set(ctx, requestIDHex, pending))

	blsMock.EXPECT().
		GetSigningStatus(ctx, requestID).
		Return(&blstypes.ThresholdSigningRequest{Status: blstypes.ThresholdSigningStatus_THRESHOLD_SIGNING_STATUS_COLLECTING_SIGNATURES}, nil)

	require.NoError(t, k.ProcessBridgeMintRefunds(ctx))

	stillPending, err := k.BridgeMintRefundsMap.Get(ctx, requestIDHex)
	require.NoError(t, err)
	require.Equal(t, pending.Creator, stillPending.Creator)
	require.Equal(t, pending.Amount, stillPending.Amount)
}
