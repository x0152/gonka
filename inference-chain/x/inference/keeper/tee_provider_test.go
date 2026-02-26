package keeper_test

import (
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/testutil"
	"github.com/productscience/inference/x/inference/types"
	"github.com/stretchr/testify/require"
)

func TestKeeper_GetTeeProviderRecords(t *testing.T) {
	k, ms, ctx := setupMsgServer(t)
	sdkCtx := sdk.UnwrapSDKContext(ctx)

	teeParticipant := NewMockAccount(testutil.Creator)
	regularParticipant := NewMockAccount(testutil.Executor)

	MustAddParticipant(t, ms, ctx, *teeParticipant)
	MustAddParticipant(t, ms, ctx, *regularParticipant)
	registerTestModels(t, k, ms, sdkCtx, "model-tee", "model-regular")

	_, err := ms.SubmitNewParticipant(ctx, &types.MsgSubmitNewParticipant{
		Creator:      testutil.Creator,
		Url:          "https://tee-participant.gonka.test",
		ValidatorKey: teeParticipant.GetPubKey().String(),
		WorkerKey:    "tee-worker-public-key",
	})
	require.NoError(t, err)

	_, err = ms.SubmitNewParticipant(ctx, &types.MsgSubmitNewParticipant{
		Creator:      testutil.Executor,
		Url:          "https://regular-participant.gonka.test",
		ValidatorKey: regularParticipant.GetPubKey().String(),
		WorkerKey:    "regular-worker-public-key",
	})
	require.NoError(t, err)

	_, err = ms.SubmitHardwareDiff(ctx, &types.MsgSubmitHardwareDiff{
		Creator: testutil.Creator,
		NewOrModified: []*types.HardwareNode{
			{
				LocalId: "tee-node-1",
				Status:  types.HardwareNodeStatus_INFERENCE,
				Models:  []string{"model-tee"},
				Hardware: []*types.Hardware{
					{Type: "TEE", Count: 1},
					{Type: "GPU", Count: 1},
				},
				Host: "tee-node.internal",
				Port: "18180",
			},
		},
		Removed: []*types.HardwareNode{},
	})
	require.NoError(t, err)

	_, err = ms.SubmitHardwareDiff(ctx, &types.MsgSubmitHardwareDiff{
		Creator: testutil.Executor,
		NewOrModified: []*types.HardwareNode{
			{
				LocalId: "gpu-node-1",
				Status:  types.HardwareNodeStatus_INFERENCE,
				Models:  []string{"model-tee"},
				Hardware: []*types.Hardware{
					{Type: "GPU", Count: 4},
				},
				Host: "gpu-node.internal",
				Port: "18080",
			},
		},
		Removed: []*types.HardwareNode{},
	})
	require.NoError(t, err)

	records, err := k.GetTeeProviderRecords(sdkCtx, "model-tee")
	require.NoError(t, err)
	require.Len(t, records, 1)

	require.Equal(t, testutil.Creator, records[0].ParticipantAddress)
	require.Equal(t, "tee-node-1", records[0].NodeLocalID)
	require.Equal(t, "model-tee", records[0].ModelID)
	require.Equal(t, "http://tee-node.internal:18180", records[0].NodeURL)
	require.Equal(t, "tee-worker-public-key", records[0].NodePublicKey)
}
