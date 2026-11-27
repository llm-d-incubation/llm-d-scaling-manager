package scalertls

import (
	"bytes"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// opaquePlaceholders are the one manifest's three Secrets: Opaque, labelled,
// no data.
func opaquePlaceholders() []client.Object {
	out := make([]client.Object, 0, 3)
	for name, role := range map[string]string{OpaqueNames.CA: "ca", OpaqueNames.Serving: "serving", OpaqueNames.Client: "client"} {
		out = append(out, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{OwnerLabel: role}},
			Type:       corev1.SecretTypeOpaque,
		})
	}
	return out
}

func opaqueEnsurer(cl client.Client, migrate bool) *Ensurer {
	e := newEnsurer(cl, fixtureNow)
	names := OpaqueNames
	e.Names = &names
	if migrate {
		legacy := LegacyNames
		e.MigrateFrom = &legacy
	}
	return e
}

// A move from an smt install: the opaque Secrets are empty and the
// kubernetes.io/tls ones hold the installer's CA and leaves. The controller
// copies them across instead of minting a CA, so KEDA, which keeps the CA of
// its first connection, stays connected; the old Secrets are not written.
// Without MigrateFrom (the negative control) a new CA is minted.
func TestEnsure_MigratesTheLegacyCA(t *testing.T) {
	for _, migrate := range []bool{true, false} {
		t.Run(map[bool]string{true: "migrate", false: "no migration"}[migrate], func(t *testing.T) {
			var c counter
			objs := append(installerSecrets(t), opaquePlaceholders()...)
			cl := newClient(t, &c, interceptor.Funcs{}, objs...)
			before := map[string]string{}
			for _, n := range []string{CASecretName, ServingSecretName, ClientSecretName} {
				before[n] = getSecret(t, cl, n).ResourceVersion
			}
			o, err := opaqueEnsurer(cl, migrate).Ensure(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			newCA := getSecret(t, cl, OpaqueNames.CA)
			sameCA := bytes.Equal(newCA.Data[corev1.TLSCertKey], readFixture(t, "ca.crt"))
			if migrate {
				if !sameCA || o.CAMinted || len(o.Migrated) != 3 {
					t.Fatalf("sameCA %v, CAMinted %v, migrated %v", sameCA, o.CAMinted, o.Migrated)
				}
				if !bytes.Equal(getSecret(t, cl, OpaqueNames.Client).Data[corev1.TLSCertKey], readFixture(t, "client.crt")) {
					t.Fatal("KEDA's client certificate was not carried over")
				}
				if o.ClientIssued || o.ServingIssued {
					t.Fatalf("issued leaves after a migration: client %v, serving %v", o.ClientIssued, o.ServingIssued)
				}
			} else if sameCA || !o.CAMinted {
				t.Fatalf("without MigrateFrom: sameCA %v, CAMinted %v", sameCA, o.CAMinted)
			}
			for n, rv := range before {
				if getSecret(t, cl, n).ResourceVersion != rv {
					t.Errorf("wrote the old Secret %s", n)
				}
			}
			// A second pass copies nothing: the new CA is no longer empty.
			o2, err := opaqueEnsurer(cl, migrate).Ensure(context.Background())
			if err != nil || len(o2.Migrated) != 0 {
				t.Fatalf("second pass migrated %v (%v)", o2.Migrated, err)
			}
		})
	}
}

// A CA rotation after the move (the rotate command empties the new CA)
// mints a new CA. The old Secrets an smt install left behind still hold the
// CA being rotated away from; copying it back would undo the rotation.
func TestEnsure_RotationAfterMigrationMints(t *testing.T) {
	var c counter
	objs := append(installerSecrets(t), opaquePlaceholders()...)
	cl := newClient(t, &c, interceptor.Funcs{}, objs...)
	if o, err := opaqueEnsurer(cl, true).Ensure(context.Background()); err != nil || len(o.Migrated) != 3 {
		t.Fatalf("migration: %v (%v)", o, err)
	}
	ca := getSecret(t, cl, OpaqueNames.CA)
	for _, k := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, corev1.ServiceAccountRootCAKey} {
		ca.Data[k] = []byte{}
	}
	if err := cl.Update(context.Background(), ca); err != nil {
		t.Fatal(err)
	}
	o, err := opaqueEnsurer(cl, true).Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Migrated) != 0 || !o.CAMinted {
		t.Fatalf("rotation after the move: migrated %v, CAMinted %v; want a new CA", o.Migrated, o.CAMinted)
	}
	if bytes.Equal(getSecret(t, cl, OpaqueNames.CA).Data[corev1.TLSCertKey], readFixture(t, "ca.crt")) {
		t.Fatal("the rotated CA is the old install's CA again")
	}
}

// A fresh install with no earlier Secrets mints, and a later rotation mints
// again even if an smt install's Secrets appear in between.
func TestEnsure_MintedCANeverMigrates(t *testing.T) {
	var c counter
	cl := newClient(t, &c, interceptor.Funcs{}, opaquePlaceholders()...)
	if o, err := opaqueEnsurer(cl, true).Ensure(context.Background()); err != nil || !o.CAMinted {
		t.Fatalf("fresh install: %v (%v)", o, err)
	}
	for _, s := range installerSecrets(t) {
		if err := cl.Create(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	ca := getSecret(t, cl, OpaqueNames.CA)
	for _, k := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, corev1.ServiceAccountRootCAKey} {
		ca.Data[k] = []byte{}
	}
	if err := cl.Update(context.Background(), ca); err != nil {
		t.Fatal(err)
	}
	o, err := opaqueEnsurer(cl, true).Ensure(context.Background())
	if err != nil || len(o.Migrated) != 0 || !o.CAMinted {
		t.Fatalf("rotation: migrated %v, CAMinted %v (%v)", o.Migrated, o.CAMinted, err)
	}
}
