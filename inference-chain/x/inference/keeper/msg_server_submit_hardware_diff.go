package keeper

import (
	"context"
	"strings"

	errorsmod "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/productscience/inference/x/inference/types"
	"golang.org/x/exp/slices"
)

func (k msgServer) SubmitHardwareDiff(goCtx context.Context, msg *types.MsgSubmitHardwareDiff) (*types.MsgSubmitHardwareDiffResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	participant, found := k.GetParticipant(goCtx, msg.Creator)
	if !found {
		return nil, types.ErrParticipantNotFound
	}

	// Check for duplicate LocalIds
	seenIds := make(map[string]bool)
	for _, node := range msg.NewOrModified {
		if seenIds[node.LocalId] {
			return nil, types.ErrDuplicateNodeId
		}
		seenIds[node.LocalId] = true
	}
	for _, node := range msg.Removed {
		if seenIds[node.LocalId] {
			return nil, types.ErrDuplicateNodeId
		}
		seenIds[node.LocalId] = true
	}

	// Make sure that before the update, we have models in the state
	for _, node := range msg.NewOrModified {
		if err := ValidateTEENode(node); err != nil {
			return nil, errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "%v", err)
		}
		if IsTEENode(node) && strings.TrimSpace(participant.WorkerPublicKey) == "" {
			return nil, errorsmod.Wrapf(
				sdkerrors.ErrInvalidRequest,
				"participant %s must provide worker_key before registering tee node %s",
				msg.Creator,
				safeLocalID(node),
			)
		}

		for _, modelId := range node.Models {
			if !k.IsValidGovernanceModel(ctx, modelId) {
				return nil, types.ErrInvalidModel
			}
		}
	}

	existingNodes, found := k.GetHardwareNodes(ctx, msg.Creator)
	if !found {
		existingNodes = &types.HardwareNodes{
			HardwareNodes: []*types.HardwareNode{},
		}
	}

	nodeMap := make(map[string]*types.HardwareNode)
	for _, node := range existingNodes.HardwareNodes {
		nodeMap[node.LocalId] = node
	}

	for _, nodeToRemove := range msg.Removed {
		delete(nodeMap, nodeToRemove.LocalId)
	}

	for _, node := range msg.NewOrModified {
		nodeMap[node.LocalId] = node
	}

	updatedNodes := &types.HardwareNodes{
		Participant:   msg.Creator,
		HardwareNodes: make([]*types.HardwareNode, 0, len(nodeMap)),
	}
	for _, node := range nodeMap {
		updatedNodes.HardwareNodes = append(updatedNodes.HardwareNodes, node)
	}
	slices.SortFunc(updatedNodes.HardwareNodes, func(a, b *types.HardwareNode) int {
		return strings.Compare(a.LocalId, b.LocalId)
	})

	k.LogInfo("Updating hardware nodes", types.Nodes, "nodes", updatedNodes)
	if err := k.SetHardwareNodes(ctx, updatedNodes); err != nil {
		k.LogError("Error setting hardware nodes", types.Nodes, "err", err)
		return nil, err
	}

	return &types.MsgSubmitHardwareDiffResponse{}, nil
}

func safeLocalID(node *types.HardwareNode) string {
	if node == nil {
		return "<nil>"
	}
	if strings.TrimSpace(node.LocalId) == "" {
		return "<empty>"
	}
	return node.LocalId
}
