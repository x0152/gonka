package types

import (
	"testing"

	errorsmod "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkbech32 "github.com/cosmos/cosmos-sdk/types/bech32"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/stretchr/testify/require"
)

// setupBech32 configures the bech32 HRP used by sdk.AccAddressFromBech32
func setupBech32() {
	// Use the same prefix seen in other tests in this repo
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonkapub")
}

func mkAddr(t *testing.T) string {
	setupBech32()
	// 20-byte address (all 1s)
	bz := make([]byte, 20)
	for i := range bz {
		bz[i] = 1
	}
	addr, err := sdkbech32.ConvertAndEncode("gonka", bz)
	require.NoError(t, err)
	return addr
}

func TestMsgUpdateParams_ValidateBasic(t *testing.T) {
	goodAuthority := mkAddr(t)

	t.Run("valid", func(t *testing.T) {
		msg := &MsgUpdateParams{
			Authority: goodAuthority,
			Params:    DefaultParams(),
		}
		require.NoError(t, msg.ValidateBasic())
	})

	t.Run("invalid authority address", func(t *testing.T) {
		msg := &MsgUpdateParams{
			Authority: "not-an-address",
			Params:    DefaultParams(),
		}
		err := msg.ValidateBasic()
		require.Error(t, err)
	})

	t.Run("invalid params", func(t *testing.T) {
		// Make params invalid: TSlotsDegreeOffset >= ITotalSlots
		p := DefaultParams()
		p.TSlotsDegreeOffset = p.ITotalSlots
		msg := &MsgUpdateParams{Authority: goodAuthority, Params: p}
		err := msg.ValidateBasic()
		require.Error(t, err)
	})
}

func TestMsgSubmitDealerPart_ValidateBasic(t *testing.T) {
	creator := mkAddr(t)
	validCommitment := make([]byte, G2CompressedSize) // 96 bytes
	validShare := []byte{0x01, 0x02}

	t.Run("valid", func(t *testing.T) {
		msg := &MsgSubmitDealerPart{
			Creator:     creator,
			EpochId:     1,
			Commitments: [][]byte{validCommitment},
			EncryptedSharesForParticipants: []EncryptedSharesForParticipant{{
				EncryptedShares: [][]byte{validShare},
			}},
		}
		require.NoError(t, msg.ValidateBasic())
	})

	t.Run("invalid creator", func(t *testing.T) {
		msg := &MsgSubmitDealerPart{Creator: "bad", EpochId: 1, Commitments: [][]byte{validCommitment}, EncryptedSharesForParticipants: []EncryptedSharesForParticipant{{EncryptedShares: [][]byte{validShare}}}}
		err := msg.ValidateBasic()
		require.Error(t, err)
		require.True(t, errorsmod.IsOf(err, sdkerrors.ErrInvalidAddress))
	})

	t.Run("epoch zero", func(t *testing.T) {
		msg := &MsgSubmitDealerPart{Creator: creator, EpochId: 0, Commitments: [][]byte{validCommitment}, EncryptedSharesForParticipants: []EncryptedSharesForParticipant{{EncryptedShares: [][]byte{validShare}}}}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("empty commitments", func(t *testing.T) {
		msg := &MsgSubmitDealerPart{Creator: creator, EpochId: 1, Commitments: nil, EncryptedSharesForParticipants: []EncryptedSharesForParticipant{{EncryptedShares: [][]byte{validShare}}}}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("empty encrypted shares list", func(t *testing.T) {
		msg := &MsgSubmitDealerPart{Creator: creator, EpochId: 1, Commitments: [][]byte{validCommitment}, EncryptedSharesForParticipants: nil}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("wrong commitment size", func(t *testing.T) {
		msg := &MsgSubmitDealerPart{Creator: creator, EpochId: 1, Commitments: [][]byte{{1}}, EncryptedSharesForParticipants: []EncryptedSharesForParticipant{{EncryptedShares: [][]byte{validShare}}}}
		require.Error(t, msg.ValidateBasic())
	})
}

func TestMsgSubmitVerificationVector_ValidateBasic(t *testing.T) {
	creator := mkAddr(t)

	t.Run("valid", func(t *testing.T) {
		msg := &MsgSubmitVerificationVector{Creator: creator, EpochId: 1, DealerValidity: []bool{true, false}}
		require.NoError(t, msg.ValidateBasic())
	})

	t.Run("invalid creator", func(t *testing.T) {
		msg := &MsgSubmitVerificationVector{Creator: "bad", EpochId: 1, DealerValidity: []bool{true}}
		err := msg.ValidateBasic()
		require.Error(t, err)
		require.True(t, errorsmod.IsOf(err, sdkerrors.ErrInvalidAddress))
	})

	t.Run("epoch zero", func(t *testing.T) {
		msg := &MsgSubmitVerificationVector{Creator: creator, EpochId: 0, DealerValidity: []bool{true}}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("empty dealer_validity", func(t *testing.T) {
		msg := &MsgSubmitVerificationVector{Creator: creator, EpochId: 1, DealerValidity: nil}
		require.Error(t, msg.ValidateBasic())
	})
}

func TestMsgSubmitGroupKeyValidationSignature_ValidateBasic(t *testing.T) {
	creator := mkAddr(t)

	t.Run("valid", func(t *testing.T) {
		msg := &MsgSubmitGroupKeyValidationSignature{
			Creator:          creator,
			NewEpochId:       1,
			SlotIndices:      []uint32{0, 2},
			PartialSignature: make([]byte, 2*ValidationG1Size), // 2 slots * 48 bytes
		}
		require.NoError(t, msg.ValidateBasic())
	})

	t.Run("invalid creator", func(t *testing.T) {
		msg := &MsgSubmitGroupKeyValidationSignature{Creator: "bad", NewEpochId: 1, SlotIndices: []uint32{0}, PartialSignature: make([]byte, ValidationG1Size)}
		err := msg.ValidateBasic()
		require.Error(t, err)
		require.True(t, errorsmod.IsOf(err, sdkerrors.ErrInvalidAddress))
	})

	t.Run("epoch zero", func(t *testing.T) {
		msg := &MsgSubmitGroupKeyValidationSignature{Creator: creator, NewEpochId: 0, SlotIndices: []uint32{0}, PartialSignature: make([]byte, ValidationG1Size)}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("empty slot indices", func(t *testing.T) {
		msg := &MsgSubmitGroupKeyValidationSignature{Creator: creator, NewEpochId: 1, SlotIndices: nil}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("wrong signature size", func(t *testing.T) {
		msg := &MsgSubmitGroupKeyValidationSignature{Creator: creator, NewEpochId: 1, SlotIndices: []uint32{0, 1}, PartialSignature: make([]byte, 10)}
		require.Error(t, msg.ValidateBasic())
	})
}

func TestMsgSubmitPartialSignature_ValidateBasic(t *testing.T) {
	creator := mkAddr(t)

	t.Run("valid", func(t *testing.T) {
		msg := &MsgSubmitPartialSignature{
			Creator:          creator,
			SlotIndices:      []uint32{1, 2},
			RequestId:        []byte{1},
			PartialSignature: make([]byte, 2*G1CompressedSize), // 2 slots * 48 bytes
		}
		require.NoError(t, msg.ValidateBasic())
	})

	t.Run("invalid creator", func(t *testing.T) {
		msg := &MsgSubmitPartialSignature{Creator: "bad", SlotIndices: []uint32{1}, RequestId: []byte{1}, PartialSignature: make([]byte, G1CompressedSize)}
		err := msg.ValidateBasic()
		require.Error(t, err)
		require.True(t, errorsmod.IsOf(err, sdkerrors.ErrInvalidAddress))
	})

	t.Run("empty slot indices", func(t *testing.T) {
		msg := &MsgSubmitPartialSignature{Creator: creator, SlotIndices: nil, RequestId: []byte{1}}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("empty request_id", func(t *testing.T) {
		msg := &MsgSubmitPartialSignature{Creator: creator, SlotIndices: []uint32{1}, RequestId: []byte{}, PartialSignature: make([]byte, G1CompressedSize)}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("wrong signature size", func(t *testing.T) {
		msg := &MsgSubmitPartialSignature{Creator: creator, SlotIndices: []uint32{1, 2}, RequestId: []byte{1}, PartialSignature: make([]byte, 10)}
		require.Error(t, msg.ValidateBasic())
	})
}

func TestMsgRequestThresholdSignature_ValidateBasic(t *testing.T) {
	creator := mkAddr(t)
	data := [][]byte{{1, 2, 3}}

	t.Run("valid", func(t *testing.T) {
		msg := &MsgRequestThresholdSignature{Creator: creator, CurrentEpochId: 1, Data: data, RequestId: []byte{1}}
		require.NoError(t, msg.ValidateBasic())
	})

	t.Run("invalid creator", func(t *testing.T) {
		msg := &MsgRequestThresholdSignature{Creator: "bad", CurrentEpochId: 1, Data: data, RequestId: []byte{1}}
		err := msg.ValidateBasic()
		require.Error(t, err)
		require.True(t, errorsmod.IsOf(err, sdkerrors.ErrInvalidAddress))
	})

	t.Run("epoch zero", func(t *testing.T) {
		msg := &MsgRequestThresholdSignature{Creator: creator, CurrentEpochId: 0, Data: data, RequestId: []byte{1}}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("empty data", func(t *testing.T) {
		msg := &MsgRequestThresholdSignature{Creator: creator, CurrentEpochId: 1, Data: nil, RequestId: []byte{1}}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("empty request_id", func(t *testing.T) {
		msg := &MsgRequestThresholdSignature{Creator: creator, CurrentEpochId: 1, Data: data, RequestId: []byte{}}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("request_id too long", func(t *testing.T) {
		msg := &MsgRequestThresholdSignature{Creator: creator, CurrentEpochId: 1, Data: data, RequestId: make([]byte, MaxRequestIDLen+1)}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("chain_id too long", func(t *testing.T) {
		msg := &MsgRequestThresholdSignature{Creator: creator, CurrentEpochId: 1, Data: data, RequestId: []byte{1}, ChainId: make([]byte, MaxChainIDLen+1)}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("too many data elements", func(t *testing.T) {
		tooManyData := make([][]byte, MaxDataElements+1)
		for i := range tooManyData {
			tooManyData[i] = []byte{1}
		}
		msg := &MsgRequestThresholdSignature{Creator: creator, CurrentEpochId: 1, Data: tooManyData, RequestId: []byte{1}}
		require.Error(t, msg.ValidateBasic())
	})

	t.Run("data element too long", func(t *testing.T) {
		bigData := [][]byte{make([]byte, MaxDataElementLen+1)}
		msg := &MsgRequestThresholdSignature{Creator: creator, CurrentEpochId: 1, Data: bigData, RequestId: []byte{1}}
		require.Error(t, msg.ValidateBasic())
	})
}
