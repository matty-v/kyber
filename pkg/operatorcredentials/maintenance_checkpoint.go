package operatorcredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	validation "k8s.io/apimachinery/pkg/util/validation"
)

const upgradeEffectsAnnotation = "kyber.io/operator-credential-upgrade-effects"

var checkpointVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// UpgradeEffectsCheckpoint records permission to begin effects, never proof
// that those effects happened or completed. No release values or key data.
type UpgradeEffectsCheckpoint struct {
	Release                string `json:"release"`
	InitialRevision        int    `json:"initialRevision"`
	TargetVersion          string `json:"targetVersion"`
	ControlPlaneDeployment string `json:"controlPlaneDeployment"`
	Authority              string `json:"authority"`
	RegistryUID            string `json:"registryUid,omitempty"`
}

func (c UpgradeEffectsCheckpoint) valid() bool {
	if len(c.Release) > 53 || len(validation.IsDNS1123Label(c.Release)) != 0 || c.InitialRevision < 1 || c.InitialRevision > 2147483647 || len(c.TargetVersion) > 128 || !checkpointVersion.MatchString(c.TargetVersion) || len(validation.IsDNS1123Subdomain(c.ControlPlaneDeployment)) != 0 {
		return false
	}
	return c.Authority == "registry" && canonicalWriterUID(c.RegistryUID) || c.Authority == "unselected" && c.RegistryUID == ""
}

type upgradeEffectsRecord struct {
	Nonce      string                   `json:"nonce"`
	Checkpoint UpgradeEffectsCheckpoint `json:"checkpoint"`
}

func decodeUpgradeEffects(raw string) (upgradeEffectsRecord, error) {
	var record upgradeEffectsRecord
	if len(raw) == 0 || len(raw) > 2048 || json.Unmarshal([]byte(raw), &record) != nil || !canonicalWriterUID(record.Nonce) || !record.Checkpoint.valid() {
		return upgradeEffectsRecord{}, ErrUnsupported
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return upgradeEffectsRecord{}, ErrUnsupported
	}
	return record, nil
}

// RecordUpgradeEffects is a one-time CAS admission before any CRD/Helm write.
// It returns a new ownership capability; the old resource version stays invalid.
// A lost acknowledgement is reconciled only inside this call using its fresh
// nonce. On uncertainty the caller must not start effects or replay this write.
func (r *MaintenanceReservation) RecordUpgradeEffects(ctx context.Context, checkpoint UpgradeEffectsCheckpoint) (*MaintenanceReservation, error) {
	if r == nil || r.store == nil || r.kind != MaintenanceUpgrade || !checkpoint.valid() {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := r.Check(ctx, MaintenanceUpgrade); err != nil {
		return nil, err
	}
	current := &corev1.Secret{}
	if err := r.store.direct.reader.Get(ctx, r.store.key, current); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	if err := r.store.validate(current); err != nil {
		return nil, err
	}
	if current.UID != r.uid || current.ResourceVersion != r.version || string(current.Data["owner"]) != r.owner || string(current.Data["kind"]) != r.kind {
		return nil, ErrConflict
	}
	if _, err := decodeMaintenanceWriter(current.Data["writer"]); err != nil {
		return nil, ErrUnsupported
	}
	if _, exists := current.Annotations[upgradeEffectsAnnotation]; exists {
		return nil, ErrConflict
	}
	record := upgradeEffectsRecord{Nonce: uuid.NewString(), Checkpoint: checkpoint}
	encoded, _ := json.Marshal(record)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[upgradeEffectsAnnotation] = string(encoded)
	writeErr := r.store.direct.writer.Update(ctx, current)
	confirmed := &corev1.Secret{}
	if err := r.store.direct.reader.Get(ctx, r.store.key, confirmed); err != nil {
		return nil, authorityUnavailable(ctx, err)
	}
	if err := r.store.validate(confirmed); err != nil {
		return nil, err
	}
	if confirmed.UID != r.uid || confirmed.ResourceVersion == r.version || string(confirmed.Data["owner"]) != r.owner || string(confirmed.Data["kind"]) != r.kind || confirmed.Annotations[upgradeEffectsAnnotation] != string(encoded) {
		if writeErr != nil {
			return nil, authorityError(ctx, writeErr)
		}
		return nil, ErrConflict
	}
	return &MaintenanceReservation{store: r.store, owner: r.owner, kind: r.kind, uid: r.uid, version: confirmed.ResourceVersion}, nil
}
