package types

import (
	errorsmod "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var _ sdk.Msg = &MsgRequestThresholdSignature{}

const (
	MaxRequestIDLen   = 32
	MaxChainIDLen     = 32
	MaxDataElements   = 20
	MaxDataElementLen = 32
)

func (m *MsgRequestThresholdSignature) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Creator); err != nil {
		return errorsmod.Wrap(sdkerrors.ErrInvalidAddress, "invalid creator address")
	}
	if m.CurrentEpochId == 0 {
		return errorsmod.Wrap(sdkerrors.ErrInvalidRequest, "current_epoch_id must be > 0")
	}
	if len(m.Data) == 0 {
		return errorsmod.Wrap(sdkerrors.ErrInvalidRequest, "data must be non-empty")
	}
	if len(m.RequestId) == 0 || len(m.RequestId) > MaxRequestIDLen {
		return errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "request_id length must be 1-%d bytes", MaxRequestIDLen)
	}
	if len(m.ChainId) > MaxChainIDLen {
		return errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "chain_id length must be <= %d bytes", MaxChainIDLen)
	}
	if len(m.Data) > MaxDataElements {
		return errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "data must have <= %d elements", MaxDataElements)
	}
	for i, d := range m.Data {
		if len(d) > MaxDataElementLen {
			return errorsmod.Wrapf(sdkerrors.ErrInvalidRequest, "data[%d] must be <= %d bytes", i, MaxDataElementLen)
		}
	}
	return nil
}
