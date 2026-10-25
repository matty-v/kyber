// kyber-credential-adopt is a fixed, non-retrying supervised Job entrypoint.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/matty-v/kyber/pkg/api"
	"k8s.io/client-go/rest"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Credential handoff could not start.")
		os.Exit(1)
	}
	metadata := api.CredentialAdoptionWorkerMetadata{Namespace: os.Getenv("KYBER_ADOPT_NAMESPACE"), PodName: os.Getenv("KYBER_ADOPT_POD_NAME"), PodUID: os.Getenv("KYBER_ADOPT_POD_UID"), DeploymentName: os.Getenv("KYBER_ADOPT_DEPLOYMENT"), OperationID: os.Getenv("KYBER_ADOPT_OPERATION_ID"), RecoveryAuthorizationEventID: os.Getenv("KYBER_ADOPT_RECOVERY_AUTHORIZATION")}
	if err := api.RunNativeCredentialAdoption(ctx, config, metadata); err != nil {
		// Do not emit source, database, Kubernetes or transport diagnostics. The
		// protected operation outcome is the administrator's source of truth.
		fmt.Fprintln(os.Stderr, "Credential handoff needs an outcome check in API access.")
		os.Exit(1)
	}
	fmt.Println("Credential handoff completed.")
}
