package roundtable

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var errAvatarNotFound = errors.New("avatar not found")

type AvatarStore interface {
	Put(ctx context.Context, key string, contentType string, body []byte) error
	Get(ctx context.Context, key string) (StoredAvatar, error)
	Delete(ctx context.Context, key string) error
}

type StoredAvatar struct {
	Content     []byte
	ContentType string
}

type LocalAvatarStore struct {
	Dir string
}

func NewLocalAvatarStore(dir string) (*LocalAvatarStore, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("avatar local dir is required")
	}
	return &LocalAvatarStore{Dir: dir}, nil
}

func (s *LocalAvatarStore) Put(ctx context.Context, key string, contentType string, body []byte) error {
	filename, err := s.filename(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return fmt.Errorf("create avatar dir: %w", err)
	}
	if err := os.WriteFile(filename, body, 0o644); err != nil {
		return fmt.Errorf("write avatar: %w", err)
	}
	return nil
}

func (s *LocalAvatarStore) Get(ctx context.Context, key string) (StoredAvatar, error) {
	filename, err := s.filename(key)
	if err != nil {
		return StoredAvatar{}, err
	}
	if err := ctx.Err(); err != nil {
		return StoredAvatar{}, err
	}
	file, err := os.Open(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StoredAvatar{}, errAvatarNotFound
		}
		return StoredAvatar{}, fmt.Errorf("read avatar: %w", err)
	}
	defer file.Close()
	body, err := readStoredAvatarBody(file)
	if err != nil {
		return StoredAvatar{}, fmt.Errorf("read avatar: %w", err)
	}
	return StoredAvatar{Content: body, ContentType: normalizedAvatarContentType}, nil
}

func (s *LocalAvatarStore) Delete(ctx context.Context, key string) error {
	filename, err := s.filename(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete avatar: %w", err)
	}
	return nil
}

func (s *LocalAvatarStore) filename(key string) (string, error) {
	if !validAvatarObjectKey(key) {
		return "", errInvalidInput("invalid avatar object key")
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	full := filepath.Join(s.Dir, clean)
	root, err := filepath.Abs(s.Dir)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", errInvalidInput("invalid avatar object key")
	}
	return target, nil
}

type S3AvatarStore struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
	HTTPClient      *http.Client
}

func NewS3AvatarStore(opts S3AvatarStore) (*S3AvatarStore, error) {
	opts.Endpoint = strings.TrimRight(strings.TrimSpace(opts.Endpoint), "/")
	opts.Region = strings.TrimSpace(opts.Region)
	opts.Bucket = strings.TrimSpace(opts.Bucket)
	opts.AccessKeyID = strings.TrimSpace(opts.AccessKeyID)
	if opts.Endpoint == "" || opts.Region == "" || opts.Bucket == "" || opts.AccessKeyID == "" || opts.SecretAccessKey == "" {
		return nil, errors.New("avatar s3 endpoint, region, bucket, access key, and secret key are required")
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	return &opts, nil
}

func (s *S3AvatarStore) Put(ctx context.Context, key string, contentType string, body []byte) error {
	req, err := s.newRequest(ctx, http.MethodPut, key, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Disposition", `inline; filename="avatar.jpg"`)
	return s.do(req, "")
}

func (s *S3AvatarStore) Get(ctx context.Context, key string) (StoredAvatar, error) {
	req, err := s.newRequest(ctx, http.MethodGet, key, nil)
	if err != nil {
		return StoredAvatar{}, err
	}
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return StoredAvatar{}, fmt.Errorf("get avatar object: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return StoredAvatar{}, errAvatarNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return StoredAvatar{}, fmt.Errorf("get avatar object: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := readStoredAvatarBody(resp.Body)
	if err != nil {
		return StoredAvatar{}, fmt.Errorf("read avatar object: %w", err)
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = normalizedAvatarContentType
	}
	return StoredAvatar{Content: body, ContentType: contentType}, nil
}

func (s *S3AvatarStore) Delete(ctx context.Context, key string) error {
	req, err := s.newRequest(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	return s.do(req, http.MethodDelete)
}

func (s *S3AvatarStore) do(req *http.Request, op string) error {
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("avatar object request: %w", err)
	}
	defer resp.Body.Close()
	if req.Method == http.MethodDelete && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if op == "" {
			op = strings.ToLower(req.Method)
		}
		return fmt.Errorf("%s avatar object: status %d: %s", op, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *S3AvatarStore) newRequest(ctx context.Context, method string, key string, body []byte) (*http.Request, error) {
	if !validAvatarObjectKey(key) {
		return nil, errInvalidInput("invalid avatar object key")
	}
	objectURL, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}
	reader := bytes.NewReader(body)
	req, err := http.NewRequestWithContext(ctx, method, objectURL, reader)
	if err != nil {
		return nil, err
	}
	s.sign(req, body, time.Now().UTC())
	return req, nil
}

func (s *S3AvatarStore) objectURL(key string) (string, error) {
	base, err := url.Parse(s.Endpoint)
	if err != nil {
		return "", fmt.Errorf("parse avatar s3 endpoint: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", errors.New("avatar s3 endpoint must be http or https")
	}
	if s.ForcePathStyle {
		base.Path = joinURLPath(base.Path, s.Bucket, key)
		return base.String(), nil
	}
	base.Host = s.Bucket + "." + base.Host
	base.Path = joinURLPath(base.Path, key)
	return base.String(), nil
}

func (s *S3AvatarStore) sign(req *http.Request, body []byte, now time.Time) {
	payloadHash := sha256Hex(body)
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Host = req.URL.Host
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalHeaders, signedHeaders := canonicalS3Headers(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := date + "/" + s.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(s.signingKey(date), []byte(stringToSign)))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.AccessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func (s *S3AvatarStore) signingKey(date string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+s.SecretAccessKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(s.Region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func canonicalS3Headers(req *http.Request) (string, string) {
	keys := make([]string, 0, len(req.Header)+1)
	values := map[string]string{}
	values["host"] = req.URL.Host
	keys = append(keys, "host")
	for name, headerValues := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" {
			continue
		}
		keys = append(keys, lower)
		values[lower] = strings.Join(headerValues, ",")
	}
	sort.Strings(keys)
	var headerBuilder strings.Builder
	for _, key := range keys {
		headerBuilder.WriteString(key)
		headerBuilder.WriteByte(':')
		headerBuilder.WriteString(strings.Join(strings.Fields(values[key]), " "))
		headerBuilder.WriteByte('\n')
	}
	return headerBuilder.String(), strings.Join(keys, ";")
}

func hmacSHA256(key []byte, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func joinURLPath(basePath string, parts ...string) string {
	segments := []string{}
	if basePath != "" && basePath != "/" {
		segments = append(segments, strings.Trim(basePath, "/"))
	}
	for _, part := range parts {
		for _, segment := range strings.Split(part, "/") {
			if segment != "" {
				segments = append(segments, url.PathEscape(segment))
			}
		}
	}
	return "/" + strings.Join(segments, "/")
}

func newAvatarObjectKey() (string, error) {
	id, err := newID("avt")
	if err != nil {
		return "", err
	}
	return "avatars/" + id + ".jpg", nil
}

func avatarOpaqueID(objectKey string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(objectKey))
}

func avatarObjectKeyFromOpaqueID(raw string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", errInvalidInput("invalid avatar id")
	}
	key := string(decoded)
	if !validAvatarObjectKey(key) {
		return "", errInvalidInput("invalid avatar id")
	}
	return key, nil
}

func validAvatarObjectKey(key string) bool {
	if !strings.HasPrefix(key, "avatars/") || strings.Contains(key, "..") || strings.ContainsAny(key, "\\\x00") {
		return false
	}
	return path.Clean(key) == key && strings.HasSuffix(key, ".jpg")
}

func readStoredAvatarBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxAvatarUploadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxAvatarUploadBytes {
		return nil, errors.New("avatar object is too large")
	}
	return body, nil
}
