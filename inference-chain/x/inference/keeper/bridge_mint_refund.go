package keeper

import (
	"context"
	"encoding/hex"
	"fmt"

	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	blstypes "github.com/productscience/inference/x/bls/types"
	"github.com/productscience/inference/x/inference/types"
)

func (k Keeper) setBridgeMintPendingRefund(ctx context.Context, blsRequestID []byte, msg *types.MsgRequestBridgeMint) error {
	if len(blsRequestID) == 0 {
		return fmt.Errorf("bls request id cannot be empty")
	}
	if msg == nil {
		return fmt.Errorf("bridge mint message cannot be nil")
	}
	return k.BridgeMintRefundsMap.Set(ctx, hex.EncodeToString(blsRequestID), *msg)
}

func (k Keeper) ProcessBridgeMintRefunds(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)

	iter, err := k.BridgeMintRefundsMap.Iterate(ctx, nil)
	if err != nil {
		return err
	}
	defer iter.Close()

	keysToCleanup := make([]string, 0)
	for ; iter.Valid(); iter.Next() {
		blsRequestIDHex, err := iter.Key()
		if err != nil {
			return err
		}
		pendingMint, err := iter.Value()
		if err != nil {
			return err
		}

		blsRequestID, err := hex.DecodeString(blsRequestIDHex)
		if err != nil {
			k.LogError("invalid pending bridge mint key", types.Messages,
				"bls_request_id", blsRequestIDHex, "error", err)
			continue
		}

		signingStatus, err := k.BlsKeeper.GetSigningStatus(sdkCtx, blsRequestID)
		if err != nil || signingStatus == nil {
			continue
		}

		switch signingStatus.Status {
		case blstypes.ThresholdSigningStatus_THRESHOLD_SIGNING_STATUS_COMPLETED:
			keysToCleanup = append(keysToCleanup, blsRequestIDHex)

		case blstypes.ThresholdSigningStatus_THRESHOLD_SIGNING_STATUS_FAILED,
			blstypes.ThresholdSigningStatus_THRESHOLD_SIGNING_STATUS_EXPIRED:
			if err := k.refundPendingBridgeMintFromEscrow(sdkCtx, &pendingMint, blsRequestIDHex, signingStatus.Status); err != nil {
				k.LogError("failed to refund bridge mint after threshold-signing failure", types.Messages,
					"bls_request_id", blsRequestIDHex, "error", err)
				continue
			}
			keysToCleanup = append(keysToCleanup, blsRequestIDHex)
		}
	}

	for _, key := range keysToCleanup {
		if err := k.BridgeMintRefundsMap.Remove(ctx, key); err != nil {
			return err
		}
	}

	return nil
}

func (k Keeper) refundPendingBridgeMintFromEscrow(
	ctx sdk.Context,
	pendingMint *types.MsgRequestBridgeMint,
	blsRequestIDHex string,
	status blstypes.ThresholdSigningStatus,
) error {
	if pendingMint == nil {
		return fmt.Errorf("pending bridge mint request is nil")
	}

	recipientAddr, err := sdk.AccAddressFromBech32(pendingMint.Creator)
	if err != nil {
		return fmt.Errorf("invalid bridge mint creator address %q: %w", pendingMint.Creator, err)
	}

	amountInt, ok := math.NewIntFromString(pendingMint.Amount)
	if !ok || !amountInt.IsPositive() {
		return fmt.Errorf("invalid bridge mint amount %q", pendingMint.Amount)
	}
	refundCoins := sdk.NewCoins(sdk.NewCoin(types.BaseCoin, amountInt))

	if err := k.ReleaseFromEscrow(ctx, recipientAddr, refundCoins); err != nil {
		return err
	}

	k.LogInfo("bridge mint escrow refunded after threshold-signing failure", types.Messages,
		"bls_request_id", blsRequestIDHex,
		"status", status.String(),
		"creator", pendingMint.Creator,
		"amount", pendingMint.Amount)

	return nil
}
