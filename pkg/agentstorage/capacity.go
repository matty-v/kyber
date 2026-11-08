// Package agentstorage describes how agent volumes consume machine capacity.
package agentstorage

import (
	"context"
	"fmt"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ExternalDisks reports whether new agent volumes use separately provisioned
// cloud disks. Unknown/local provisioners retain local disk accounting. An empty
// class name follows Kubernetes' default-class selection (newest default wins).
// Missing classes are unknown; API failures must not silently disable limits.
func ExternalDisks(ctx context.Context, reader client.Reader, className string) (bool, error) {
	var class storagev1.StorageClass
	if className != "" {
		if err := reader.Get(ctx, types.NamespacedName{Name: className}, &class); err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("reading agent storage class: %w", err)
		}
	} else {
		var classes storagev1.StorageClassList
		if err := reader.List(ctx, &classes); err != nil {
			return false, fmt.Errorf("listing default storage classes: %w", err)
		}
		for _, candidate := range classes.Items {
			if candidate.Annotations["storageclass.kubernetes.io/is-default-class"] != "true" &&
				candidate.Annotations["storageclass.beta.kubernetes.io/is-default-class"] != "true" {
				continue
			}
			if class.Name == "" || candidate.CreationTimestamp.After(class.CreationTimestamp.Time) ||
				(candidate.CreationTimestamp.Equal(&class.CreationTimestamp) && candidate.Name < class.Name) {
				class = candidate
			}
		}
	}
	return class.Provisioner == "pd.csi.storage.gke.io" || class.Provisioner == "ebs.csi.aws.com" ||
		class.Provisioner == "kubernetes.io/gce-pd" || class.Provisioner == "kubernetes.io/aws-ebs", nil
}
