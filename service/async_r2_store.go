package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const AsyncObjectMaxBytes int64 = 256 << 20

var asyncMediaProtection = &common.SSRFProtection{AllowedPorts: []int{80, 443}, ApplyIPFilterForDomain: true}

var asyncMediaHTTPClient = &http.Client{
	Timeout: 2 * time.Minute,
	Transport: &http.Transport{
		DialContext: (&protectedFetchDialer{
			resolver:      net.DefaultResolver,
			dialContext:   (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			getProtection: func() (*common.SSRFProtection, bool, error) { return asyncMediaProtection, true, nil },
		}).DialContext,
		MaxIdleConns: 8, MaxIdleConnsPerHost: 2, IdleConnTimeout: time.Minute,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
	},
	CheckRedirect: func(request *http.Request, previous []*http.Request) error {
		if len(previous) >= 5 {
			return errors.New("async artifact redirect limit exceeded")
		}
		return validateAsyncArtifactURL(request.URL)
	},
}

func validateAsyncArtifactURL(target *url.URL) error {
	if target == nil || target.User != nil || target.Fragment != "" || target.Opaque != "" {
		return errors.New("invalid async artifact URL")
	}
	return asyncMediaProtection.ValidateURL(target.String())
}

// AsyncArtifactHTTPClient returns the mandatory direct, DNS-protected client
// for trusted native artifact requests that may require provider headers.
func AsyncArtifactHTTPClient() *http.Client {
	client := *asyncMediaHTTPClient
	return &client
}

func ValidateAsyncArtifactURL(rawURL string) error {
	target, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("invalid async artifact URL")
	}
	return validateAsyncArtifactURL(target)
}

// DownloadAsyncArtifact is always SSRF protected, including DNS rebinding and
// every redirect. Legacy fetch/proxy/TLS bypass settings cannot weaken it.
func DownloadAsyncArtifact(ctx context.Context, rawURL string) (*http.Response, error) {
	target, err := url.Parse(rawURL)
	if err != nil || validateAsyncArtifactURL(target) != nil {
		return nil, errors.New("async artifact URL was blocked")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("invalid async artifact request")
	}
	response, err := asyncMediaHTTPClient.Do(request)
	if err != nil {
		return nil, errors.New("async artifact download failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || response.ContentLength > AsyncObjectMaxBytes {
		response.Body.Close()
		return nil, errors.New("async artifact download was rejected")
	}
	return response, nil
}

type AsyncR2Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Prefix    string
}

var asyncR2BucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// InitAsyncObjectStoreFromEnv does not contact R2 or print configuration values.
// A partial/invalid configuration disables only the additive async API.
func InitAsyncObjectStoreFromEnv() error {
	SetAsyncObjectStore(nil)
	config := AsyncR2Config{
		Endpoint: os.Getenv("ASYNC_R2_ENDPOINT"), Bucket: os.Getenv("ASYNC_R2_BUCKET"),
		AccessKey: os.Getenv("ASYNC_R2_ACCESS_KEY_ID"), SecretKey: os.Getenv("ASYNC_R2_SECRET_ACCESS_KEY"),
		Prefix: os.Getenv("ASYNC_R2_PREFIX"),
	}
	if config.Endpoint == "" && config.Bucket == "" && config.AccessKey == "" && config.SecretKey == "" {
		return nil
	}
	store, err := NewAsyncR2Store(config)
	if err != nil {
		return err
	}
	SetAsyncObjectStore(&trackedAsyncObjectStore{AsyncObjectStore: store})
	return nil
}

func NewAsyncR2Store(config AsyncR2Config) (AsyncObjectStore, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Port() != "" ||
		!strings.HasSuffix(endpoint.Hostname(), ".r2.cloudflarestorage.com") ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") {
		return nil, errors.New("ASYNC_R2_ENDPOINT must be an HTTPS Cloudflare R2 S3 endpoint")
	}
	if !asyncR2BucketPattern.MatchString(config.Bucket) {
		return nil, errors.New("ASYNC_R2_BUCKET is missing or invalid")
	}
	for _, credential := range []string{config.AccessKey, config.SecretKey} {
		if credential == "" || len(credential) > 1024 || credential != strings.TrimSpace(credential) || strings.ContainsFunc(credential, unicode.IsControl) {
			return nil, errors.New("R2 access credentials are missing or invalid")
		}
	}
	if config.Prefix == "" {
		config.Prefix = "new-api-async"
	}
	if !validAsyncObjectKey(config.Prefix) {
		return nil, errors.New("ASYNC_R2_PREFIX is invalid")
	}
	endpoint.Path = ""
	transport := (&http.Transport{TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second}).Clone()
	if defaults, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaults.Clone()
	}
	// The endpoint is trusted deployment configuration, never a caller URL.
	// Do not inherit an ambient proxy carrying unrelated local credentials.
	transport.Proxy = nil
	return &asyncR2Store{
		endpoint: endpoint, bucket: config.Bucket, prefix: config.Prefix,
		credentials: aws.Credentials{AccessKeyID: config.AccessKey, SecretAccessKey: config.SecretKey},
		client:      &http.Client{Transport: transport, Timeout: 3 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		signer:      v4.NewSigner(),
	}, nil
}

type asyncR2Store struct {
	endpoint    *url.URL
	bucket      string
	prefix      string
	credentials aws.Credentials
	client      *http.Client
	signer      *v4.Signer
}

func (*asyncR2Store) Enabled() bool { return true }

func validAsyncObjectKey(key string) bool {
	if key == "" || len(key) > 768 || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\\?#") || strings.ContainsFunc(key, unicode.IsControl) {
		return false
	}
	for part := range strings.SplitSeq(key, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (s *asyncR2Store) request(ctx context.Context, method, key, contentType string, data []byte) (*http.Response, error) {
	if !validAsyncObjectKey(key) {
		return nil, errors.New("invalid async object key")
	}
	objectURL := *s.endpoint
	objectURL.Path = "/" + s.bucket + "/" + s.prefix + "/" + key
	request, err := http.NewRequestWithContext(ctx, method, objectURL.String(), bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("could not create R2 object request")
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	hash := sha256.Sum256(data)
	payloadHash := hex.EncodeToString(hash[:])
	request.Header.Set("x-amz-content-sha256", payloadHash)
	if err := s.signer.SignHTTP(ctx, s.credentials, request, payloadHash, "s3", "auto", time.Now()); err != nil {
		return nil, errors.New("could not sign R2 object request")
	}
	response, err := s.client.Do(request)
	if err != nil {
		// A transport error may contain a URL. Do not expose infrastructure or credentials.
		return nil, errors.New("R2 object request failed")
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 || method == http.MethodDelete && response.StatusCode == http.StatusNotFound {
		return response, nil
	}
	response.Body.Close()
	return nil, fmt.Errorf("R2 object request returned HTTP %d", response.StatusCode)
}

func (s *asyncR2Store) Put(ctx context.Context, key, contentType string, reader io.Reader, maxBytes int64) (AsyncObjectRef, error) {
	if !validAsyncObjectKey(key) || maxBytes <= 0 || maxBytes > AsyncObjectMaxBytes {
		return AsyncObjectRef{}, errors.New("invalid async object key or size limit")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if strings.ContainsAny(contentType, "\r\n") || len(contentType) > 255 {
		return AsyncObjectRef{}, errors.New("invalid async object content type")
	}
	// Bound memory before upload. Never spill media to a local durable directory.
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return AsyncObjectRef{}, errors.New("could not read async object content")
	}
	if int64(len(data)) > maxBytes {
		return AsyncObjectRef{}, errors.New("async object exceeds size limit")
	}
	response, err := s.request(ctx, http.MethodPut, key, contentType, data)
	if err != nil {
		return AsyncObjectRef{}, err
	}
	response.Body.Close()
	return AsyncObjectRef{Backend: "r2", Key: key, ContentType: contentType, Size: int64(len(data))}, nil
}

func (s *asyncR2Store) Open(ctx context.Context, ref AsyncObjectRef) (io.ReadCloser, string, int64, error) {
	if ref.Backend != "r2" || ref.Size < 0 || ref.Size > AsyncObjectMaxBytes {
		return nil, "", 0, errors.New("invalid async object reference")
	}
	response, err := s.request(ctx, http.MethodGet, ref.Key, "", nil)
	if err != nil {
		return nil, "", 0, err
	}
	if response.ContentLength >= 0 && response.ContentLength != ref.Size {
		response.Body.Close()
		return nil, "", 0, errors.New("R2 object size does not match its durable reference")
	}
	return response.Body, ref.ContentType, ref.Size, nil
}

func (s *asyncR2Store) Lookup(ctx context.Context, key string, maxBytes int64) (AsyncObjectRef, error) {
	if !validAsyncObjectKey(key) || maxBytes <= 0 || maxBytes > AsyncObjectMaxBytes {
		return AsyncObjectRef{}, errors.New("invalid async lookup limit")
	}
	response, err := s.request(ctx, http.MethodHead, key, "", nil)
	if err != nil {
		return AsyncObjectRef{}, err
	}
	defer response.Body.Close()
	if response.ContentLength < 0 || response.ContentLength > maxBytes {
		return AsyncObjectRef{}, errors.New("async recovery object size is missing or exceeds its limit")
	}
	return AsyncObjectRef{Backend: "r2", Key: key, ContentType: response.Header.Get("Content-Type"), Size: response.ContentLength}, nil
}

func (s *asyncR2Store) Delete(ctx context.Context, ref AsyncObjectRef) error {
	if ref.Backend != "r2" {
		return errors.New("invalid async object reference")
	}
	response, err := s.request(ctx, http.MethodDelete, ref.Key, "", nil)
	if err != nil {
		return err
	}
	response.Body.Close()
	return nil
}
