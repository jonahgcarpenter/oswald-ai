// Package vault owns the profile-scoped encrypted credential store for
// browser autofill. Metadata (kind, label, origin, login identifiers) is
// agent-visible; secret payloads (passwords, authenticator seeds) are
// resolved server-side and never enter tool results, logs, or history.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Item kinds. Login and address items are fillable; payment items can be
// stored but never filled (card entry needs an interactive confirmation
// that no session currently provides, so filling refuses fail-closed).
const (
	KindLogin   = "login"
	KindAddress = "address"
	KindPayment = "payment"
)

// Login identifier types. The identifier is metadata, not a secret: the
// agent sees it and types it itself. Only the password stays sealed.
var LoginIdentifierTypes = []string{"email", "phone", "username"}

const (
	keyFileName   = "vault.key"
	vaultFileName = "vault.json.enc"
	lockFileName  = "vault.lock"
	maxSecretLen  = 4096
)

// VaultError is a failure safe to surface (never contains secret values).
type VaultError struct{ msg string }

func (e *VaultError) Error() string { return e.msg }

func vaultErr(format string, args ...interface{}) *VaultError {
	return &VaultError{msg: fmt.Sprintf(format, args...)}
}

// Canonical secret-payload fields per non-login kind. Each maps to the
// WHATWG autocomplete token the fill targets.
var AddressFields = map[string]string{
	"address_line1": "address-line1", "address_line2": "address-line2",
	"city": "address-level2", "state": "address-level1",
	"postal_code": "postal-code", "country": "country-name",
}

var PaymentFields = map[string]string{
	"card_number": "cc-number", "cardholder_name": "cc-name",
	"exp_month": "cc-exp-month", "exp_year": "cc-exp-year",
	"cvc": "cc-csc", "billing_postal_code": "postal-code",
}

var requiredFields = map[string][]string{
	KindAddress: {"address_line1", "city", "postal_code", "country"},
	KindPayment: {"card_number", "exp_month", "exp_year", "cvc"},
}

// VaultItemMeta is the metadata-only view of an item. It never contains
// secret values.
type VaultItemMeta struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Label          string `json:"label"`
	Origin         string `json:"origin"`
	CreatedAt      string `json:"created_at"`
	IdentifierType string `json:"identifier_type,omitempty"`
	Identifier     string `json:"identifier,omitempty"`
	HasOTP         bool   `json:"has_otp,omitempty"`
}

// Store is an encrypted, profile-scoped vault under <profileRoot>/vault/.
type Store struct {
	mu        sync.Mutex
	dir       string
	keyPath   string
	vaultPath string
	lockPath  string
}

// NewStore binds a vault to one profile root. It performs no I/O.
func NewStore(profileRoot string) *Store {
	dir := filepath.Join(profileRoot, "vault")
	return &Store{
		dir:       dir,
		keyPath:   filepath.Join(dir, keyFileName),
		vaultPath: filepath.Join(dir, vaultFileName),
		lockPath:  filepath.Join(dir, lockFileName),
	}
}

type record struct {
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	Label          string            `json:"label"`
	Origin         string            `json:"origin"`
	CreatedAt      string            `json:"created_at"`
	IdentifierType string            `json:"identifier_type,omitempty"`
	Identifier     string            `json:"identifier,omitempty"`
	Secret         map[string]string `json:"secret"`
}

func (s *Store) locked(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *Store) sealKey() ([]byte, error) {
	if data, err := os.ReadFile(s.keyPath); err == nil {
		if len(data) != 32 {
			return nil, vaultErr("vault key has an unexpected size")
		}
		if err := os.Chmod(s.keyPath, 0600); err != nil {
			return nil, err
		}
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	return key, f.Close()
}

func (s *Store) readAll() ([]record, error) {
	data, err := os.ReadFile(s.vaultPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	key, err := s.sealKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, vaultErr("vault file is corrupted")
	}
	raw, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return nil, vaultErr("vault file could not be decrypted (key mismatch or corruption)")
	}
	var payload struct {
		Version int      `json:"version"`
		Items   []record `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, vaultErr("vault file is corrupted")
	}
	return payload.Items, nil
}

func (s *Store) writeAll(items []record) error {
	key, err := s.sealKey()
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Version int      `json:"version"`
		Items   []record `json:"items"`
	}{Version: 1, Items: items})
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	blob := gcm.Seal(nonce, nonce, raw, nil)
	tmp, err := os.CreateTemp(s.dir, ".vault-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.vaultPath)
}

func metaOf(rec record) VaultItemMeta {
	return VaultItemMeta{
		ID: rec.ID, Kind: rec.Kind, Label: rec.Label, Origin: rec.Origin,
		CreatedAt: rec.CreatedAt, IdentifierType: rec.IdentifierType,
		Identifier: rec.Identifier, HasOTP: rec.Secret["otp_seed"] != "",
	}
}

// AddLogin stores a login bound to one origin. The identifier lives in
// metadata; only the password (and optional authenticator seed) is sealed.
func (s *Store) AddLogin(label, identifierType, identifier, password, otpSeed, origin string) (VaultItemMeta, error) {
	if strings.TrimSpace(label) == "" {
		return VaultItemMeta{}, vaultErr("label is required")
	}
	validType := false
	for _, t := range LoginIdentifierTypes {
		if identifierType == t {
			validType = true
		}
	}
	if !validType {
		return VaultItemMeta{}, vaultErr("identifier_type must be one of email, phone, username")
	}
	if strings.TrimSpace(identifier) == "" || password == "" {
		return VaultItemMeta{}, vaultErr("login items require identifier and password")
	}
	if len(password) > maxSecretLen {
		return VaultItemMeta{}, vaultErr("password exceeds the size limit")
	}
	normOrigin, err := NormalizeOrigin(origin)
	if err != nil {
		return VaultItemMeta{}, err
	}
	secret := map[string]string{"password": password}
	if strings.TrimSpace(otpSeed) != "" {
		seed, err := NormalizeOTPSecret(otpSeed)
		if err != nil {
			return VaultItemMeta{}, err
		}
		secret["otp_seed"] = seed
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return VaultItemMeta{}, err
	}
	rec := record{
		ID: "vault_" + hex.EncodeToString(id), Kind: KindLogin,
		Label: strings.TrimSpace(label), Origin: normOrigin,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		IdentifierType: identifierType, Identifier: strings.TrimSpace(identifier),
		Secret: secret,
	}
	var out VaultItemMeta
	if err := s.locked(func() error {
		items, err := s.readAll()
		if err != nil {
			return err
		}
		items = append(items, rec)
		if err := s.writeAll(items); err != nil {
			return err
		}
		out = metaOf(rec)
		return nil
	}); err != nil {
		return VaultItemMeta{}, err
	}
	return out, nil
}

// List returns metadata for every item. Secret payloads are never included.
func (s *Store) List() ([]VaultItemMeta, error) {
	var out []VaultItemMeta
	if err := s.locked(func() error {
		items, err := s.readAll()
		if err != nil {
			return err
		}
		for _, rec := range items {
			out = append(out, metaOf(rec))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if out == nil {
		out = []VaultItemMeta{}
	}
	return out, nil
}

// GetMeta returns one item's metadata, or nil when the handle is unknown.
func (s *Store) GetMeta(id string) (*VaultItemMeta, error) {
	var out *VaultItemMeta
	if err := s.locked(func() error {
		items, err := s.readAll()
		if err != nil {
			return err
		}
		for _, rec := range items {
			if rec.ID == id {
				meta := metaOf(rec)
				out = &meta
				return nil
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolvePassword returns the sealed payload for server-side use only.
// Callers must never place the values into tool results, logs, or history.
func (s *Store) ResolvePassword(id string) (password, otpSeed string, err error) {
	if err := s.locked(func() error {
		items, err := s.readAll()
		if err != nil {
			return err
		}
		for _, rec := range items {
			if rec.ID == id {
				if rec.Kind != KindLogin {
					return vaultErr("vault item %q is not a login", id)
				}
				password = rec.Secret["password"]
				otpSeed = rec.Secret["otp_seed"]
				if password == "" {
					return vaultErr("vault item %q has no password", id)
				}
				return nil
			}
		}
		return vaultErr("no vault item with handle %q", id)
	}); err != nil {
		return "", "", err
	}
	return password, otpSeed, nil
}

// AddAddress stores an address bound to one origin. Field names are the
// canonical AddressFields keys; unknown or blank values are dropped.
func (s *Store) AddAddress(label string, fields map[string]string, origin string) (VaultItemMeta, error) {
	return s.addCheckout(KindAddress, label, fields, origin)
}

// AddPayment stores a payment card bound to one origin. Cards can be
// listed but never filled; filling refuses fail-closed.
func (s *Store) AddPayment(label string, fields map[string]string, origin string) (VaultItemMeta, error) {
	return s.addCheckout(KindPayment, label, fields, origin)
}

func (s *Store) addCheckout(kind, label string, fields map[string]string, origin string) (VaultItemMeta, error) {
	allowed := AddressFields
	if kind == KindPayment {
		allowed = PaymentFields
	}
	if strings.TrimSpace(label) == "" {
		return VaultItemMeta{}, vaultErr("label is required")
	}
	secret := map[string]string{}
	for k, v := range fields {
		if _, ok := allowed[k]; !ok {
			continue
		}
		if v = strings.TrimSpace(v); v == "" || len(v) > maxSecretLen {
			continue
		}
		secret[k] = v
	}
	var missing []string
	for _, f := range requiredFields[kind] {
		if secret[f] == "" {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return VaultItemMeta{}, vaultErr("%s items require %s", kind, strings.Join(missing, ", "))
	}
	normOrigin, err := NormalizeOrigin(origin)
	if err != nil {
		return VaultItemMeta{}, err
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return VaultItemMeta{}, err
	}
	rec := record{
		ID: "vault_" + hex.EncodeToString(id), Kind: kind,
		Label: strings.TrimSpace(label), Origin: normOrigin,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Secret: secret,
	}
	var out VaultItemMeta
	if err := s.locked(func() error {
		items, err := s.readAll()
		if err != nil {
			return err
		}
		items = append(items, rec)
		if err := s.writeAll(items); err != nil {
			return err
		}
		out = metaOf(rec)
		return nil
	}); err != nil {
		return VaultItemMeta{}, err
	}
	return out, nil
}

// ResolveFields returns the sealed payload of an address or payment item
// for server-side use only. Login items resolve through ResolvePassword.
func (s *Store) ResolveFields(id string) (map[string]string, error) {
	var out map[string]string
	if err := s.locked(func() error {
		items, err := s.readAll()
		if err != nil {
			return err
		}
		for _, rec := range items {
			if rec.ID == id {
				if rec.Kind == KindLogin {
					return vaultErr("vault item %q is a login; resolve it as one", id)
				}
				out = dictCopy(rec.Secret)
				return nil
			}
		}
		return vaultErr("no vault item with handle %q", id)
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func dictCopy(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Remove deletes one item, reporting whether it existed.
func (s *Store) Remove(id string) (bool, error) {
	removed := false
	if err := s.locked(func() error {
		items, err := s.readAll()
		if err != nil {
			return err
		}
		kept := items[:0]
		for _, rec := range items {
			if rec.ID == id {
				removed = true
				continue
			}
			kept = append(kept, rec)
		}
		if !removed {
			return nil
		}
		return s.writeAll(kept)
	}); err != nil {
		return false, err
	}
	return removed, nil
}

// NormalizeOrigin reduces a URL or origin to scheme://host[:port].
// Default ports are stripped so equivalent origins compare equal.
func NormalizeOrigin(urlOrOrigin string) (string, error) {
	value := strings.TrimSpace(urlOrOrigin)
	if value == "" {
		return "", vaultErr("origin is required")
	}
	if !strings.Contains(value, "://") {
		return "", vaultErr("origin must include a scheme")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", vaultErr("could not parse origin")
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if scheme == "" || host == "" {
		return "", vaultErr("could not parse origin")
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port == "" {
		return fmt.Sprintf("%s://%s", scheme, host), nil
	}
	return fmt.Sprintf("%s://%s:%s", scheme, host, port), nil
}

// NormalizeOTPSecret accepts a bare base32 seed or an otpauth://totp URI
// and returns the canonical stored form: the bare seed, or
// seed|digits|period|algo when the URI departs from the 6/30/SHA1 defaults.
func NormalizeOTPSecret(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", vaultErr("authenticator key must not be empty")
	}
	digits, period, algo := 6, 30, "SHA1"
	if strings.HasPrefix(strings.ToLower(value), "otpauth://") {
		parsed, err := url.Parse(value)
		if err != nil || !strings.EqualFold(parsed.Host, "totp") {
			return "", vaultErr("only otpauth://totp links are supported")
		}
		query := parsed.Query()
		value = query.Get("secret")
		if digitsRaw := query.Get("digits"); digitsRaw != "" {
			var n int
			if _, err := fmt.Sscanf(digitsRaw, "%d", &n); err != nil || (n != 6 && n != 7 && n != 8) {
				return "", vaultErr("unsupported otpauth digits (6-8)")
			}
			digits = n
		}
		if periodRaw := query.Get("period"); periodRaw != "" {
			var n int
			if _, err := fmt.Sscanf(periodRaw, "%d", &n); err != nil || n <= 0 {
				return "", vaultErr("unsupported otpauth period")
			}
			period = n
		}
		if algoRaw := query.Get("algorithm"); algoRaw != "" {
			upper := strings.ToUpper(strings.ReplaceAll(algoRaw, "-", ""))
			switch upper {
			case "SHA1", "SHA256", "SHA512":
				algo = upper
			default:
				return "", vaultErr("unsupported otpauth algorithm (SHA1/SHA256/SHA512)")
			}
		}
	}
	seed := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(value, " ", ""), "-", ""))
	seed = strings.TrimRight(seed, "=")
	if seed == "" {
		return "", vaultErr("authenticator key must be a base32 secret")
	}
	for _, c := range seed {
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			return "", vaultErr("authenticator key must be a base32 secret")
		}
	}
	if digits == 6 && period == 30 && algo == "SHA1" {
		return seed, nil
	}
	return fmt.Sprintf("%s|%d|%d|%s", seed, digits, period, algo), nil
}

// totpParams splits the canonical stored seed into its parameters.
func totpParams(seed string) (raw string, digits, period int, algo string) {
	digits, period, algo = 6, 30, "SHA1"
	raw = seed
	if parts := strings.Split(seed, "|"); len(parts) == 4 {
		raw = parts[0]
		fmt.Sscanf(parts[1], "%d", &digits)
		fmt.Sscanf(parts[2], "%d", &period)
		switch strings.ToUpper(parts[3]) {
		case "SHA256":
			algo = "SHA256"
		case "SHA512":
			algo = "SHA512"
		}
	}
	return raw, digits, period, algo
}

// TOTPNow mints the current RFC 6238 code for a stored seed. Stdlib only.
func TOTPNow(seed string, at time.Time) (string, error) {
	raw, digits, period, algo := totpParams(seed)
	padded := raw + strings.Repeat("=", (-len(raw))&7)
	key, err := base32.StdEncoding.DecodeString(padded)
	if err != nil {
		return "", vaultErr("authenticator key is not valid base32")
	}
	var digest []byte
	counter := uint64(at.Unix() / int64(period))
	msg := []byte{
		byte(counter >> 56), byte(counter >> 48), byte(counter >> 40), byte(counter >> 32),
		byte(counter >> 24), byte(counter >> 16), byte(counter >> 8), byte(counter),
	}
	var newHash func() hash.Hash
	switch algo {
	case "SHA256":
		newHash = sha256.New
	case "SHA512":
		newHash = sha512.New
	default:
		newHash = sha1.New
	}
	mac := hmac.New(newHash, key)
	mac.Write(msg)
	digest = mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0F
	code := (uint32(digest[offset])&0x7F)<<24 | uint32(digest[offset+1])<<16 |
		uint32(digest[offset+2])<<8 | uint32(digest[offset+3])
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	format := fmt.Sprintf("%%0%dd", digits)
	return fmt.Sprintf(format, code%mod), nil
}
