// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// resolveEnv merges spec.env with spec.secretEnv, reading the referenced
// Secrets from the Model's own namespace (D63).
//
// Three rules, each of which exists because the alternative fails silently
// and expensively:
//
//   - A name in BOTH env and secretEnv is an ERROR, not a precedence
//     decision. Whichever won would be invisible in the CR, and guessing
//     wrong on a credential is not a recoverable mistake.
//   - A missing Secret or key is an ERROR. Sending an empty value instead
//     produces a run that provisions, bills, and then fails to download its
//     weights — which reads as a broken model, not a missing credential.
//     Same failure shape as D54.
//   - The resolved values are returned to the caller and never written to
//     status, never logged, and never stored on the Model.
func resolveEnv(ctx context.Context, c client.Client, model *squallv1alpha1.Model) (map[string]string, error) {
	if len(model.Spec.SecretEnv) == 0 {
		return model.Spec.Env, nil
	}

	// Deterministic order so the error a user sees is stable across
	// reconciles rather than whichever map key came out first.
	names := make([]string, 0, len(model.Spec.SecretEnv))
	for name := range model.Spec.SecretEnv {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]string, len(model.Spec.Env)+len(names))
	for k, v := range model.Spec.Env {
		out[k] = v
	}

	cache := map[string]*corev1.Secret{}
	for _, envName := range names {
		if _, clash := model.Spec.Env[envName]; clash {
			return nil, fmt.Errorf(
				"env %q is set in both spec.env and spec.secretEnv: remove one, this is not a precedence question",
				envName)
		}
		ref := model.Spec.SecretEnv[envName]

		secret, ok := cache[ref.Name]
		if !ok {
			secret = &corev1.Secret{}
			key := types.NamespacedName{Namespace: model.Namespace, Name: ref.Name}
			if err := c.Get(ctx, key, secret); err != nil {
				return nil, fmt.Errorf("env %q: read Secret %s: %w", envName, key, err)
			}
			cache[ref.Name] = secret
		}

		value, present := secret.Data[ref.Key]
		if !present {
			return nil, fmt.Errorf(
				"env %q: Secret %s/%s has no key %q",
				envName, model.Namespace, ref.Name, ref.Key)
		}
		out[envName] = string(value)
	}
	return out, nil
}
