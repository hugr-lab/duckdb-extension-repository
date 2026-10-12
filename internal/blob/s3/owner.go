package s3

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// ownerTransport sends x-amz-expected-bucket-owner on every request (spec 0005 phase 3a): S3 then
// refuses a bucket another account owns, such as one deleted and re-created by someone else. S3
// requires every x-amz-* header to be signed and minio-go has no option for a header on every
// request, so the transport adds it and signs the request again with the same credentials, payload
// hash and time. minio-go streams a signed payload only over http, which an AWS identity never
// uses; such a request is refused rather than sent unsigned.
// It sends nothing to a host but the configured one (or its virtual-hosted form): minio-go rebuilds
// AWS hosts from its own region map, and an AWS identity goes only to the endpoint config names.
type ownerTransport struct {
	base   http.RoundTripper
	owner  string
	region string
	hosts  map[string]bool
	creds  aws.CredentialsProvider
	signer *v4.Signer
}

func newOwnerTransport(base http.RoundTripper, owner, region, endpoint, bucket string, creds aws.CredentialsProvider) *ownerTransport {
	endpoint = strings.ToLower(endpoint)
	return &ownerTransport{base: base, owner: owner, region: region, creds: creds,
		hosts:  map[string]bool{endpoint: true, strings.ToLower(bucket) + "." + endpoint: true},
		signer: v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })}
}

const expectedOwner = "X-Amz-Expected-Bucket-Owner"

func (t *ownerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.hosts[strings.ToLower(req.URL.Host)] || req.URL.Scheme != "https" {
		return nil, errors.New("blob/s3: a request to another host than the configured AWS S3 endpoint")
	}
	payload := req.Header.Get("X-Amz-Content-Sha256")
	if !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || payload == "" ||
		strings.HasPrefix(payload, "STREAMING-") || strings.HasSuffix(payload, "-TRAILER") {
		return nil, errors.New("blob/s3: a request kista cannot sign with the expected bucket owner")
	}
	at, err := time.Parse("20060102T150405Z", req.Header.Get("X-Amz-Date"))
	if err != nil {
		return nil, errors.New("blob/s3: a request without its signing time")
	}
	c, err := t.creds.Retrieve(req.Context())
	if err != nil {
		return nil, fmt.Errorf("%w: the AWS identity's credentials could not be obtained", errCredentials)
	}
	r := req.Clone(req.Context())
	r.Header.Set(expectedOwner, t.owner)
	r.Header.Del("Authorization")
	if err := t.signer.SignHTTP(r.Context(), c, r, payload, "s3", t.region, at); err != nil {
		return nil, errors.New("blob/s3: signing a request failed")
	}
	return t.base.RoundTrip(r)
}
