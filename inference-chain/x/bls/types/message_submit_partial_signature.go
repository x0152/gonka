package types

import (
	errorsmod "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var _ sdk.Msg = &MsgSubmitPartialSignature{}

const (
	G1CompressedSize        = 48
	MaxSlotIndices          = 200
	MaxPartialSignatureSize = 200 * G1CompressedSize // one G1 per slot
)

func (m *MsgSubmitPartialSignature) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Creator); err != nil {
		return errorsmod.Wrap(sdkerrors.ErrInvalidAddress, "invalid creator address")
	}
	if len(m.SlotIndices) == 0 {
		return errorsmod.Wrap(sdkerrors.ErrInvalidRequest, "slot_indices must be non-empty")
	}
	if len(m.SlotIndices) > MaxSlotIndices {
		return errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "slot_indices count exceeds max %d", MaxSlotIndices)
	}
	if len(m.RequestId) == 0 || len(m.RequestId) > 32 {
		return errorsmod.Wrap(sdkerrors.ErrInvalidRequest, "request_id must be 1-32 bytes")
	}
	expectedSigSize := len(m.SlotIndices) * G1CompressedSize
	if len(m.PartialSignature) != expectedSigSize {
		return errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "partial_signature must be %d bytes (48 per slot)", expectedSigSize)
	}
	return nil
}
