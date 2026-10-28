package api

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type privateAdoptionDatabaseURL struct{ value string }

func (privateAdoptionDatabaseURL) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "credential handoff database [redacted]")
}
func (privateAdoptionDatabaseURL) MarshalJSON() ([]byte, error) {
	return nil, operatorcredentials.ErrSensitive
}

// Follow the supported chart's ordered environment expansion. A later password
// declaration cannot retroactively change an already-expanded URL. Read only
// database references; never copy the control plane's other provider credentials.
func readAdoptionDatabaseURL(ctx context.Context, reader client.Reader, namespace string, container corev1.Container) (privateAdoptionDatabaseURL, error) {
	fail := func() (privateAdoptionDatabaseURL, error) {
		return privateAdoptionDatabaseURL{}, operatorcredentials.ErrUnavailable
	}
	if reader == nil || namespace == "" || len(container.EnvFrom) != 0 {
		return fail()
	}
	password, defined, dsn := "", false, ""
	expand := func(value string) (string, bool) {
		var output strings.Builder
		for i := 0; i < len(value); {
			if value[i] == '$' && i+1 < len(value) && value[i+1] == '$' {
				output.WriteByte('$')
				i += 2
				continue
			}
			if strings.HasPrefix(value[i:], "$(POSTGRES_PASSWORD)") {
				if !defined {
					return "", false
				}
				output.WriteString(password)
				i += len("$(POSTGRES_PASSWORD)")
				continue
			}
			if strings.HasPrefix(value[i:], "$(") {
				return "", false
			}
			output.WriteByte(value[i])
			i++
		}
		return output.String(), true
	}
	for _, env := range container.Env {
		if strings.HasPrefix(env.Name, "PG") {
			return fail()
		}
		if env.Name != "POSTGRES_PASSWORD" && env.Name != "KYBER_POSTGRES_URL" {
			continue
		}
		value := env.Value
		if env.ValueFrom != nil {
			ref := env.ValueFrom.SecretKeyRef
			if value != "" || ref == nil || env.ValueFrom.ConfigMapKeyRef != nil || env.ValueFrom.FieldRef != nil || env.ValueFrom.ResourceFieldRef != nil || ref.Name == "" || ref.Key == "" || ref.Optional != nil && *ref.Optional {
				return fail()
			}
			secret := &corev1.Secret{}
			bounded, cancel := context.WithTimeout(ctx, credentialSourceReadTimeout)
			err := reader.Get(bounded, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret)
			expired := bounded.Err() != nil
			cancel()
			if err != nil || expired || secret.Namespace != namespace || secret.Name != ref.Name || secret.DeletionTimestamp != nil {
				return fail()
			}
			data, exists := secret.Data[ref.Key]
			if !exists {
				return fail()
			}
			value = string(data)
		} else {
			var ok bool
			value, ok = expand(value)
			if !ok {
				return fail()
			}
		}
		if len(value) > 64<<10 || strings.IndexByte(value, 0) >= 0 {
			return fail()
		}
		if env.Name == "POSTGRES_PASSWORD" {
			password = value
			defined = true
		} else {
			dsn = value
		}
	}
	if ctx.Err() != nil || dsn == "" {
		return fail()
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" || parsed.Fragment != "" {
		return fail()
	}
	return privateAdoptionDatabaseURL{value: dsn}, nil
}
