package types

// TeeProviderRecord is a derived view built from participant + hardware node data.
// It is intentionally kept as a regular Go struct (not protobuf) to avoid
// introducing new wire-level module APIs for the first integration step.
type TeeProviderRecord struct {
	ParticipantAddress string
	NodeLocalID        string
	ModelID            string
	NodeURL            string
	NodePublicKey      string
}
