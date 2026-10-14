package awskms_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/awskms"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// Against moto (KISTA_TEST_MOTO=http://127.0.0.1:<port>, spec 0004 phase 3): a key moto creates
// passes every metadata, tag and public-key check, and a key without the tag does not. moto hashes
// a DIGEST message again, so the contract must refuse its signature as one that does not verify.
func TestMoto(t *testing.T) {
	endpoint := os.Getenv("KISTA_TEST_MOTO")
	if endpoint == "" {
		if os.Getenv("KISTA_TEST_REQUIRE_MOTO") == "1" {
			t.Fatal("KISTA_TEST_MOTO is not set")
		}
		t.Skip("KISTA_TEST_MOTO is not set")
	}
	creds := credentials.NewStaticCredentialsProvider("testing", "testing", "")
	admin := kms.New(kms.Options{Region: region, Credentials: creds, BaseEndpoint: aws.String(endpoint)})
	create := func(tag string) string {
		in := &kms.CreateKeyInput{KeySpec: types.KeySpecRsa2048, KeyUsage: types.KeyUsageTypeSignVerify}
		if tag != "" {
			in.Tags = []types.Tag{{TagKey: aws.String(awskms.PurposeTag), TagValue: aws.String(tag)}}
		}
		out, err := admin.CreateKey(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.KeyMetadata.AWSAccountId) != account {
			t.Skipf("moto's account is %s", aws.ToString(out.KeyMetadata.AWSAccountId))
		}
		return aws.ToString(out.KeyMetadata.KeyId)
	}
	src, err := awskms.New(region, account, false, creds, awskms.Options{Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	reg := keysource.NewRegistry(signer.Resolver{})
	if err := reg.Add("moto", src, keysource.Options{Allow: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	good, untagged := create(keysource.PurposeChannel), create("")
	sg, err := reg.Open(t.Context(), "moto:"+good)
	if err != nil {
		t.Fatalf("open a tagged RSA_2048 key: %v", err)
	}
	if _, err := reg.Open(t.Context(), "moto:"+untagged); !errors.Is(err, keysource.ErrKey) {
		t.Fatalf("an untagged key: %v", err)
	}
	if _, err := reg.OpenPurpose(t.Context(), "moto:"+good, keysource.PurposeAttestation); !errors.Is(err, keysource.ErrKey) {
		t.Fatalf("a channel key as an attestation key: %v", err)
	}
	if _, err := admin.DisableKey(t.Context(), &kms.DisableKeyInput{KeyId: aws.String(good)}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Open(t.Context(), "moto:"+good); !errors.Is(err, keysource.ErrKey) {
		t.Fatalf("a disabled key: %v", err)
	}
	if _, err := admin.EnableKey(t.Context(), &kms.EnableKeyInput{KeyId: aws.String(good)}); err != nil {
		t.Fatal(err)
	}
	// moto's signature is over SHA-256(digest), not the digest: refused, never stored
	if _, err := sg.Sign(t.Context(), [32]byte{7}); !errors.Is(err, keysource.ErrSign) || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("moto's double-hashed signature: %v", err)
	}
}
