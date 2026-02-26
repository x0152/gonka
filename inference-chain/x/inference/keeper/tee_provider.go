package keeper

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/productscience/inference/x/inference/types"
)

func IsTEENode(node *types.HardwareNode) bool {
	if node == nil {
		return false
	}
	for _, hw := range node.Hardware {
		if hw == nil {
			continue
		}
		switch normalizeHardwareType(hw.Type) {
		case "TEE", "TDX", "SEV-SNP", "SEV_SNP", "CONFIDENTIAL":
			return true
		}
	}
	return false
}

func BuildNodeURL(node *types.HardwareNode) (string, bool) {
	if node == nil {
		return "", false
	}
	host := strings.TrimSpace(node.Host)
	port := strings.TrimSpace(node.Port)
	if host == "" {
		return "", false
	}

	// Host can be provided either as plain host/IP or full URL.
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		u, err := url.Parse(host)
		if err != nil || u.Host == "" {
			return "", false
		}
		if port != "" && !strings.Contains(u.Host, ":") {
			u.Host = fmt.Sprintf("%s:%s", u.Host, port)
		}
		return strings.TrimRight(u.String(), "/"), true
	}

	if port == "" {
		return "", false
	}
	return fmt.Sprintf("http://%s:%s", host, port), true
}

func ValidateTEENode(node *types.HardwareNode) error {
	if !IsTEENode(node) {
		return nil
	}
	if node == nil {
		return fmt.Errorf("tee node is nil")
	}
	if strings.TrimSpace(node.LocalId) == "" {
		return fmt.Errorf("tee node local_id is required")
	}
	if len(node.Models) == 0 {
		return fmt.Errorf("tee node %q must contain at least one model", node.LocalId)
	}
	if strings.TrimSpace(node.Host) == "" {
		return fmt.Errorf("tee node %q host is required", node.LocalId)
	}
	if strings.TrimSpace(node.Port) == "" {
		return fmt.Errorf("tee node %q port is required", node.LocalId)
	}
	port, err := strconv.Atoi(strings.TrimSpace(node.Port))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("tee node %q has invalid port: %s", node.LocalId, node.Port)
	}
	return nil
}

func (k Keeper) GetTeeProviderRecords(ctx context.Context, modelID string) ([]*types.TeeProviderRecord, error) {
	allNodes, err := k.GetAllHardwareNodes(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*types.TeeProviderRecord, 0)
	for _, participantNodes := range allNodes {
		if participantNodes == nil {
			continue
		}
		participant, found := k.GetParticipant(ctx, participantNodes.Participant)
		if !found || strings.TrimSpace(participant.WorkerPublicKey) == "" {
			continue
		}

		for _, node := range participantNodes.HardwareNodes {
			if !IsTEENode(node) {
				continue
			}
			nodeURL, ok := BuildNodeURL(node)
			if !ok {
				continue
			}

			for _, nodeModel := range node.Models {
				if modelID != "" && nodeModel != modelID {
					continue
				}
				result = append(result, &types.TeeProviderRecord{
					ParticipantAddress: participantNodes.Participant,
					NodeLocalID:        node.LocalId,
					ModelID:            nodeModel,
					NodeURL:            nodeURL,
					NodePublicKey:      participant.WorkerPublicKey,
				})
			}
		}
	}

	return result, nil
}

func normalizeHardwareType(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}
