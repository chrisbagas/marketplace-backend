// Package s3 is a minimal client for S3-compatible object storage (Cloudflare R2
// in production, a local S3 server in dev). It covers only what the app needs:
// presigned PUT URLs for browser uploads, plus HEAD / ranged GET / PUT / DELETE
// from the server. Requests use AWS Signature Version 4 and path-style
// addressing (<endpoint>/<bucket>/<key>), which R2, MinIO and AWS all accept.
//
// Reference: https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-authenticating-requests.html
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	algorithm   = "AWS4-HMAC-SHA256"
	service     = "s3"
	unsignedPay = "UNSIGNED-PAYLOAD"
)

// Config comes from the environment (see FromEnv).
type Config struct {
	Endpoint        string // e.g. https://<account>.r2.cloudflarestorage.com or http://localhost:9000
	Region          string // R2: "auto"
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	PublicURL       string // where browsers read objects: https://files.<domain> or http://localhost:9000/<bucket>
}

type Client struct {
	cfg  Config
	base *url.URL
	HTTP *http.Client
}

// ErrNotFound is returned when the object does not exist.
var ErrNotFound = errors.New("s3: object not found")

func New(cfg Config) (*Client, error) {
	base, err := url.Parse(cfg.Endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("S3_ENDPOINT tidak valid: %q", cfg.Endpoint)
	}
	if cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("S3_BUCKET, S3_ACCESS_KEY_ID, dan S3_SECRET_ACCESS_KEY wajib diisi")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if cfg.PublicURL == "" {
		return nil, errors.New("FILES_PUBLIC_URL wajib diisi")
	}
	return &Client{cfg: cfg, base: base, HTTP: &http.Client{Timeout: 60 * time.Second}}, nil
}

// Dev defaults point at the MinIO-compatible service in docker-compose.yml.
const (
	devEndpoint = "http://localhost:9000"
	devBucket   = "karyakita-files"
	devKey      = "karyakita"
	devSecret   = "karyakita_dev"
)

// FromEnv builds the client from S3_* / FILES_PUBLIC_URL. Outside production,
// unset variables fall back to the docker-compose dev storage. In production
// the variables are mandatory: it returns (nil, nil) when none of them is set
// (uploads disabled) and an error when only some are.
func FromEnv(production bool) (*Client, error) {
	cfg := Config{
		Endpoint:        os.Getenv("S3_ENDPOINT"),
		Region:          os.Getenv("S3_REGION"),
		Bucket:          os.Getenv("S3_BUCKET"),
		AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		PublicURL:       os.Getenv("FILES_PUBLIC_URL"),
	}
	if production {
		if cfg == (Config{}) {
			return nil, nil
		}
		return New(cfg)
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = devEndpoint
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Bucket == "" {
		cfg.Bucket = devBucket
	}
	if cfg.AccessKeyID == "" {
		cfg.AccessKeyID = devKey
	}
	if cfg.SecretAccessKey == "" {
		cfg.SecretAccessKey = devSecret
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = strings.TrimRight(cfg.Endpoint, "/") + "/" + cfg.Bucket
	}
	return New(cfg)
}

// PublicBase is FILES_PUBLIC_URL without the trailing slash.
func (c *Client) PublicBase() string { return c.cfg.PublicURL }

// PublicURL is the address browsers use to read an object.
func (c *Client) PublicURL(key string) string { return c.cfg.PublicURL + "/" + key }

// KeyFromURL is the inverse of PublicURL; ok is false for foreign URLs.
func (c *Client) KeyFromURL(u string) (key string, ok bool) {
	key, ok = strings.CutPrefix(u, c.cfg.PublicURL+"/")
	return key, ok && key != ""
}

// ---- presign ------------------------------------------------------------

// PresignPut returns a URL the browser can PUT the object to for the next
// `expires`. The content type and exact length are part of the signature, so
// the browser must send the same Content-Type and a body of exactly `size`
// bytes (it sets Content-Length itself).
func (c *Client) PresignPut(key, contentType string, size int64, expires time.Duration) string {
	headers := map[string]string{
		"content-length": strconv.FormatInt(size, 10),
		"content-type":   contentType,
	}
	return presignURL("PUT", c.base.Scheme, c.base.Host, c.objectPath(key), headers,
		c.cfg.AccessKeyID, c.cfg.SecretAccessKey, c.cfg.Region, expires, time.Now())
}

// presignURL builds a query-string-signed URL. headers are the extra headers
// that must be sent with the request (host is always signed).
func presignURL(method, scheme, host, path string, headers map[string]string,
	accessKey, secret, region string, expires time.Duration, now time.Time) string {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	scope := date + "/" + region + "/" + service + "/aws4_request"

	signed := map[string]string{"host": host}
	for k, v := range headers {
		signed[strings.ToLower(k)] = v
	}
	names := sortedKeys(signed)

	query := map[string]string{
		"X-Amz-Algorithm":     algorithm,
		"X-Amz-Credential":    accessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.Itoa(int(expires.Seconds())),
		"X-Amz-SignedHeaders": strings.Join(names, ";"),
	}
	canonQuery := canonicalQuery(query)
	canonical := strings.Join([]string{
		method, path, canonQuery, canonicalHeaders(signed, names), strings.Join(names, ";"), unsignedPay,
	}, "\n")
	sig := signature(secret, date, region, amzDate, scope, canonical)
	return scheme + "://" + host + path + "?" + canonQuery + "&X-Amz-Signature=" + sig
}

// ---- server-side requests ----------------------------------------------

type ObjectInfo struct {
	Size        int64
	ContentType string
}

func (c *Client) Head(ctx context.Context, key string) (ObjectInfo, error) {
	resp, err := c.do(ctx, http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer resp.Body.Close()
	if err := statusErr(resp, "HEAD"); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Size: resp.ContentLength, ContentType: resp.Header.Get("Content-Type")}, nil
}

// GetPrefix reads up to the first n bytes of the object (ranged GET).
func (c *Client) GetPrefix(ctx context.Context, key string, n int64) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, key, map[string]string{"range": "bytes=0-" + strconv.FormatInt(n-1, 10)}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := statusErr(resp, "GET"); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(resp.Body, n))
}

func (c *Client) Put(ctx context.Context, key, contentType string, data []byte) error {
	resp, err := c.do(ctx, http.MethodPut, key, map[string]string{"content-type": contentType}, data, bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return statusErr(resp, "PUT")
}

// Delete removes the object; a missing object is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return statusErr(resp, "DELETE")
}

func statusErr(resp *http.Response, op string) error {
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("s3 %s: %s %s", op, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (c *Client) objectPath(key string) string {
	return strings.TrimRight(c.base.Path, "/") + "/" + uriEncode(c.cfg.Bucket, false) + "/" + uriEncode(key, false)
}

// do sends a header-signed request. payload is only used for its sha256.
func (c *Client) do(ctx context.Context, method, key string, extra map[string]string, payload []byte, body io.Reader) (*http.Response, error) {
	path := c.objectPath(key)
	req, err := http.NewRequestWithContext(ctx, method, c.base.Scheme+"://"+c.base.Host+path, body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(sum[:])
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	scope := date + "/" + c.cfg.Region + "/" + service + "/aws4_request"

	signed := map[string]string{"host": c.base.Host, "x-amz-content-sha256": payloadHash, "x-amz-date": amzDate}
	for k, v := range extra {
		signed[strings.ToLower(k)] = v
	}
	names := sortedKeys(signed)
	canonical := strings.Join([]string{method, path, "", canonicalHeaders(signed, names), strings.Join(names, ";"), payloadHash}, "\n")
	sig := signature(c.cfg.SecretAccessKey, date, c.cfg.Region, amzDate, scope, canonical)

	for k, v := range extra {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, c.cfg.AccessKeyID, scope, strings.Join(names, ";"), sig))
	return c.HTTP.Do(req)
}

// ---- SigV4 helpers ------------------------------------------------------

func signature(secret, date, region, amzDate, scope, canonicalRequest string) string {
	hash := sha256.Sum256([]byte(canonicalRequest))
	toSign := algorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(hash[:])
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	return hex.EncodeToString(hmacSHA256(k, toSign))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func canonicalHeaders(h map[string]string, names []string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + ":" + strings.Join(strings.Fields(h[n]), " ") + "\n")
	}
	return b.String()
}

func canonicalQuery(q map[string]string) string {
	keys := sortedKeys(q)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = uriEncode(k, true) + "=" + uriEncode(q[k], true)
	}
	return strings.Join(parts, "&")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// uriEncode is the S3 flavour of percent-encoding: unreserved characters stay,
// "/" stays only in object paths.
func uriEncode(s string, encodeSlash bool) string {
	const hexUpper = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		case ch == '/' && !encodeSlash:
			b.WriteByte(ch)
		default:
			b.WriteByte('%')
			b.WriteByte(hexUpper[ch>>4])
			b.WriteByte(hexUpper[ch&15])
		}
	}
	return b.String()
}
