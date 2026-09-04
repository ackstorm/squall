// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"golang.org/x/crypto/ssh"
)

// SSHKeySecretName is the Secret holding squall's OWN SSH keypair, in the
// controller's namespace. The controller mints it; squall-proxy reads the
// private half to dial replicas.
const SSHKeySecretName = "squall-replica-ssh-key"

// Keys WITHIN the Secret, not credentials themselves — gosec's G101 pattern
// matches the filename shape, not any secret material.
const (
	sshKeySecretPrivateKey = "id_ed25519"     // #nosec G101 -- a map key, not a credential
	sshKeySecretPublicKey  = "id_ed25519.pub" // #nosec G101 -- a map key, not a credential
)

// EnsureSSHKey returns squall's public key in authorized_keys form, minting
// the keypair on first use and reusing it thereafter.
//
// Why squall has a key at all: dstack's vastai backend builds a container's
// authorized_keys from BOTH run_spec.ssh_key_pub and its own project key
// (core/backends/vastai/compute.py). Squall builds the run spec, so supplying
// its own public key here is what later lets squall-proxy reach the replica
// directly — WITHOUT ever handling dstack's project private key, which is one
// long-lived credential for the whole project, stored unencrypted in dstack's
// database (D47).
//
// ed25519 rather than RSA: small, fast, no key-size decision to get wrong, and
// accepted by every OpenSSH this touches.
//
// FAILS OPEN, deliberately. A key that cannot be minted or read returns an
// empty string and no error, and the caller simply applies without one —
// dstack then substitutes the calling user's key and squall keeps using
// dstack's own service proxy, which works everywhere. Wiring a faster data
// path must never be able to prevent a wake: `0->1` fails open.
//
// The keypair is per-INSTALLATION, not per-run. Per-run ephemeral keys are
// better (nothing outlives the replica that trusts it) and are the intended
// end state, but they need the private half to reach squall-proxy for that
// specific run, which is a handoff this version does not have. Rotation
// therefore costs a re-provision, since authorized_keys is written at
// container creation.
func EnsureSSHKey(ctx context.Context, c client.Client, namespace string) string {
	key := types.NamespacedName{Namespace: namespace, Name: SSHKeySecretName}

	var existing corev1.Secret
	err := c.Get(ctx, key, &existing)
	if err == nil {
		if pub, ok := existing.Data[sshKeySecretPublicKey]; ok && len(pub) > 0 {
			return string(pub)
		}
		// Present but unusable. Refuse to "repair" it: another squall may own
		// it, and overwriting the private half would silently strand every
		// replica already trusting the old one.
		return ""
	}
	if !apierrors.IsNotFound(err) {
		return ""
	}

	pub, priv, err := generateSSHKeypair()
	if err != nil {
		return ""
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SSHKeySecretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "squall",
				"app.kubernetes.io/component": "replica-ssh-key",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			sshKeySecretPrivateKey: priv,
			sshKeySecretPublicKey:  pub,
		},
	}
	if err := c.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost a race with another replica of this controller. Whoever won
			// wrote a valid pair; re-read rather than clobber it.
			if err := c.Get(ctx, key, &existing); err == nil {
				return string(existing.Data[sshKeySecretPublicKey])
			}
		}
		return ""
	}
	return string(pub)
}

// generateSSHKeypair returns (authorized_keys line, PEM private key).
func generateSSHKeypair() (pub, priv []byte, err error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, nil, fmt.Errorf("wrap public key: %w", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(privKey, "squall")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key: %w", err)
	}
	return ssh.MarshalAuthorizedKey(sshPub), pem.EncodeToMemory(pemBlock), nil
}
