package types

import (
	errorsmod "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var _ sdk.Msg = &MsgSubmitDealerPart{}

const (
	G2CompressedSize   = 96
	MaxCommitments     = 200
	MaxParticipants    = 200
	MaxEncryptedShares = 1000
)

func (m *MsgSubmitDealerPart) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Creator); err != nil {
		return errorsmod.Wrap(sdkerrors.ErrInvalidAddress, "invalid creator address")
	}
	if m.EpochId == 0 {
		return errorsmod.Wrap(sdkerrors.ErrInvalidRequest, "epoch_id must be > 0")
	}
	if len(m.Commitments) == 0 {
		return errorsmod.Wrap(sdkerrors.ErrInvalidRequest, "commitments must be non-empty")
	}
	if len(m.Commitments) > MaxCommitments {
		return errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "commitments count exceeds max %d", MaxCommitments)
	}
	for i, c := range m.Commitments {
		if len(c) != G2CompressedSize {
			return errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "commitment[%d] must be %d bytes", i, G2CompressedSize)
		}
	}
	if len(m.EncryptedSharesForParticipants) == 0 {
		return errorsmod.Wrap(sdkerrors.ErrInvalidRequest, "encrypted_shares_for_participants must be non-empty")
	}
	if len(m.EncryptedSharesForParticipants) > MaxParticipants {
		return errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "participants count exceeds max %d", MaxParticipants)
	}
	return nil
}
