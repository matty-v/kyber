package agentstorage

import (
	"context"
	"errors"
	"testing"
	"time"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestExternalDisks(t *testing.T) {
	class := func(name, provisioner string, defaultClass bool, created int64) *storagev1.StorageClass {
		annotations := map[string]string{}
		if defaultClass {
			annotations["storageclass.kubernetes.io/is-default-class"] = "true"
		}
		return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations,
			CreationTimestamp: metav1.NewTime(time.Unix(created, 0))}, Provisioner: provisioner}
	}
	tests := []struct {
		name, selected string
		classes        []runtime.Object
		want           bool
	}{
		{"named PD", "cloud", []runtime.Object{class("cloud", "pd.csi.storage.gke.io", false, 0)}, true},
		{"legacy PD", "cloud", []runtime.Object{class("cloud", "kubernetes.io/gce-pd", false, 0)}, true},
		{"legacy EBS", "cloud", []runtime.Object{class("cloud", "kubernetes.io/aws-ebs", false, 0)}, true},
		{"named EBS", "cloud", []runtime.Object{class("cloud", "ebs.csi.aws.com", false, 0)}, true},
		{"default PD", "", []runtime.Object{class("cloud", "pd.csi.storage.gke.io", true, 0)}, true},
		{"local", "local", []runtime.Object{class("local", "rancher.io/local-path", true, 0)}, false},
		{"unknown driver", "custom", []runtime.Object{class("custom", "custom.example.com", true, 0)}, false},
		{"missing named class", "missing", []runtime.Object{class("cloud", "pd.csi.storage.gke.io", true, 0)}, false},
		{"no default", "", []runtime.Object{class("cloud", "pd.csi.storage.gke.io", false, 0)}, false},
		{"newest default local wins", "", []runtime.Object{class("cloud", "pd.csi.storage.gke.io", true, 1), class("local", "rancher.io/local-path", true, 2)}, false},
		{"named overrides default", "cloud", []runtime.Object{class("cloud", "pd.csi.storage.gke.io", false, 0), class("local", "rancher.io/local-path", true, 2)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := storagev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tc.classes...).Build()
			got, err := ExternalDisks(context.Background(), reader, tc.selected)
			if err != nil || got != tc.want {
				t.Fatalf("ExternalDisks = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestExternalDisksReadFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("API unavailable")
	reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return unavailable
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return unavailable
		},
	}).Build()
	for _, name := range []string{"", "cloud"} {
		external, err := ExternalDisks(context.Background(), reader, name)
		if external || !errors.Is(err, unavailable) {
			t.Fatalf("ExternalDisks(%q) = %v, %v; want wrapped read failure", name, external, err)
		}
	}
}
