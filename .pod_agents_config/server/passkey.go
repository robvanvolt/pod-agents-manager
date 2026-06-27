package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

type passkeyCredential struct {
	ID         string   `json:"id"`
	PublicKey  string   `json:"public_key"`
	SignCount  uint32   `json:"sign_count"`
	Transports []string `json:"transports,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	Label      string   `json:"label,omitempty"`
}

type passkeySummary struct {
	ID         string   `json:"id"`
	Label      string   `json:"label,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	Transports []string `json:"transports,omitempty"`
}

type passkeyChallenge struct {
	Challenge string `json:"challenge,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type webAuthnCredentialResponse struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Response struct {
		ClientDataJSON    string   `json:"clientDataJSON"`
		AttestationObject string   `json:"attestationObject"`
		AuthenticatorData string   `json:"authenticatorData"`
		Signature         string   `json:"signature"`
		Transports        []string `json:"transports"`
	} `json:"response"`
}

type webAuthnClientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

type parsedRegistrationAuthData struct {
	CredentialID  []byte
	PublicKeyCOSE []byte
	SignCount     uint32
	Alg           int
}

type cborParser struct {
	data []byte
	pos  int
}

func passkeyStatus(root string) map[string]any {
	state, err := loadAuthState(root)
	count := 0
	if err == nil {
		count = len(state.Passkeys)
	}
	return map[string]any{
		"enabled":              count > 0,
		"registered":           count,
		"browserPackage":       "@simplewebauthn/browser",
		"browserVersion":       "13.3.0",
		"serverImplementation": "native-go-webauthn-compatible",
		"secureContext":        "required",
	}
}

func listPasskeys(root string) ([]passkeySummary, error) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return nil, err
	}
	keys := make([]passkeySummary, 0, len(state.Passkeys))
	for _, key := range state.Passkeys {
		keys = append(keys, passkeySummary{
			ID:         key.ID,
			Label:      key.Label,
			CreatedAt:  key.CreatedAt,
			LastUsedAt: key.LastUsedAt,
			Transports: key.Transports,
		})
	}
	return keys, nil
}

func passkeyRegistrationOptions(root string, r *http.Request) (map[string]any, error) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return nil, err
	}
	challenge, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	if state.PasskeyUserID == "" {
		state.PasskeyUserID, err = randomToken(16)
		if err != nil {
			return nil, err
		}
	}
	state.PasskeyRegistrationChallenge = passkeyChallenge{
		Challenge: challenge,
		ExpiresAt: time.Now().Add(5 * time.Minute).Format(time.RFC3339),
	}
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := saveAuthState(root, state); err != nil {
		return nil, err
	}

	exclude := []map[string]any{}
	for _, key := range state.Passkeys {
		exclude = append(exclude, map[string]any{
			"id":         key.ID,
			"type":       "public-key",
			"transports": key.Transports,
		})
	}
	userName := "pod operator"
	return map[string]any{
		"challenge": challenge,
		"rp": map[string]any{
			"name": "Pod Agents Manager",
			"id":   webAuthnRPID(r),
		},
		"user": map[string]any{
			"id":          state.PasskeyUserID,
			"name":        userName,
			"displayName": userName,
		},
		"pubKeyCredParams":   []map[string]any{{"type": "public-key", "alg": -7}},
		"timeout":            60000,
		"attestation":        "none",
		"excludeCredentials": exclude,
		"authenticatorSelection": map[string]any{
			"residentKey":      "preferred",
			"userVerification": "preferred",
		},
	}, nil
}

func readPasskeyManageRequest(r *http.Request) (string, string, error) {
	var body struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") {
		if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&body); err != nil {
			return "", "", fmt.Errorf("invalid JSON body")
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return "", "", fmt.Errorf("bad form data")
		}
		body.ID = r.FormValue("id")
		body.Label = r.FormValue("label")
	}

	body.ID = strings.TrimSpace(body.ID)
	body.Label = strings.TrimSpace(body.Label)
	if body.ID == "" {
		return "", "", fmt.Errorf("passkey id is required")
	}
	if len(body.Label) > 80 {
		return "", "", fmt.Errorf("passkey label is too long")
	}
	return body.ID, body.Label, nil
}

func renamePasskey(root, id, label string) error {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return err
	}
	for i := range state.Passkeys {
		if state.Passkeys[i].ID == id {
			state.Passkeys[i].Label = label
			state.UpdatedAt = time.Now().Format(time.RFC3339)
			return saveAuthState(root, state)
		}
	}
	return fmt.Errorf("passkey not found")
}

func deletePasskey(root, id string) error {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return err
	}
	next := state.Passkeys[:0]
	deleted := false
	for _, key := range state.Passkeys {
		if key.ID == id {
			deleted = true
			continue
		}
		next = append(next, key)
	}
	if !deleted {
		return fmt.Errorf("passkey not found")
	}
	state.Passkeys = next
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveAuthState(root, state)
}

func verifyPasskeyRegistration(root string, r *http.Request) error {
	var credential webAuthnCredentialResponse
	if err := json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&credential); err != nil {
		return fmt.Errorf("invalid JSON body")
	}
	if credential.Type != "public-key" {
		return fmt.Errorf("invalid credential type")
	}
	clientDataJSON, err := base64URLDecode(credential.Response.ClientDataJSON)
	if err != nil {
		return fmt.Errorf("invalid clientDataJSON")
	}
	var clientData webAuthnClientData
	if err := json.Unmarshal(clientDataJSON, &clientData); err != nil {
		return fmt.Errorf("invalid client data")
	}

	authMutex.Lock()
	defer authMutex.Unlock()
	state, err := loadAuthState(root)
	if err != nil {
		return err
	}
	if err := verifyPasskeyChallenge(state.PasskeyRegistrationChallenge, clientData, "webauthn.create", expectedOrigin(r)); err != nil {
		return err
	}

	attestationObject, err := base64URLDecode(credential.Response.AttestationObject)
	if err != nil {
		return fmt.Errorf("invalid attestationObject")
	}
	authData, err := authenticatorDataFromAttestation(attestationObject)
	if err != nil {
		return err
	}
	parsed, err := parseRegistrationAuthData(authData, webAuthnRPID(r))
	if err != nil {
		return err
	}
	if parsed.Alg != -7 {
		return fmt.Errorf("unsupported passkey public key algorithm")
	}
	id := base64.RawURLEncoding.EncodeToString(parsed.CredentialID)
	for _, existing := range state.Passkeys {
		if existing.ID == id {
			return fmt.Errorf("passkey already registered")
		}
	}
	now := time.Now().Format(time.RFC3339)
	transports := credential.Response.Transports
	state.Passkeys = append(state.Passkeys, passkeyCredential{
		ID:         id,
		PublicKey:  base64.RawURLEncoding.EncodeToString(parsed.PublicKeyCOSE),
		SignCount:  parsed.SignCount,
		Transports: transports,
		CreatedAt:  now,
		Label:      "Pod Dashboard passkey",
	})
	state.PasskeyRegistrationChallenge = passkeyChallenge{}
	state.UpdatedAt = now
	return saveAuthState(root, state)
}

func passkeyAuthenticationOptions(root string, r *http.Request) (map[string]any, error) {
	authMutex.Lock()
	defer authMutex.Unlock()

	state, err := loadAuthState(root)
	if err != nil {
		return nil, err
	}
	if len(state.Passkeys) == 0 {
		return nil, fmt.Errorf("no passkeys registered")
	}
	challenge, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	state.PasskeyAuthenticationChallenge = passkeyChallenge{
		Challenge: challenge,
		ExpiresAt: time.Now().Add(5 * time.Minute).Format(time.RFC3339),
	}
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := saveAuthState(root, state); err != nil {
		return nil, err
	}

	allow := []map[string]any{}
	for _, key := range state.Passkeys {
		allow = append(allow, map[string]any{
			"id":         key.ID,
			"type":       "public-key",
			"transports": key.Transports,
		})
	}
	return map[string]any{
		"challenge":        challenge,
		"timeout":          60000,
		"rpId":             webAuthnRPID(r),
		"allowCredentials": allow,
		"userVerification": "preferred",
	}, nil
}

func verifyPasskeyAuthenticationAndCreateSession(root string, r *http.Request) (string, error) {
	var credential webAuthnCredentialResponse
	if err := json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&credential); err != nil {
		return "", fmt.Errorf("invalid JSON body")
	}
	if credential.Type != "public-key" {
		return "", fmt.Errorf("invalid credential type")
	}
	credentialID := credential.RawID
	if credentialID == "" {
		credentialID = credential.ID
	}
	clientDataJSON, err := base64URLDecode(credential.Response.ClientDataJSON)
	if err != nil {
		return "", fmt.Errorf("invalid clientDataJSON")
	}
	var clientData webAuthnClientData
	if err := json.Unmarshal(clientDataJSON, &clientData); err != nil {
		return "", fmt.Errorf("invalid client data")
	}
	authData, err := base64URLDecode(credential.Response.AuthenticatorData)
	if err != nil {
		return "", fmt.Errorf("invalid authenticatorData")
	}
	signature, err := base64URLDecode(credential.Response.Signature)
	if err != nil {
		return "", fmt.Errorf("invalid signature")
	}

	authMutex.Lock()
	defer authMutex.Unlock()
	state, err := loadAuthState(root)
	if err != nil {
		return "", err
	}
	if err := verifyPasskeyChallenge(state.PasskeyAuthenticationChallenge, clientData, "webauthn.get", expectedOrigin(r)); err != nil {
		return "", err
	}
	keyIndex := -1
	for i, key := range state.Passkeys {
		if key.ID == credentialID {
			keyIndex = i
			break
		}
	}
	if keyIndex < 0 {
		return "", fmt.Errorf("unknown passkey")
	}
	signCount, err := verifyAuthenticationData(authData, clientDataJSON, signature, state.Passkeys[keyIndex], webAuthnRPID(r))
	if err != nil {
		return "", err
	}
	if state.Passkeys[keyIndex].SignCount > 0 && signCount > 0 && signCount <= state.Passkeys[keyIndex].SignCount {
		return "", fmt.Errorf("passkey sign count did not increase")
	}

	session, err := randomToken(32)
	if err != nil {
		return "", err
	}
	now := time.Now()
	state.Passkeys[keyIndex].SignCount = signCount
	state.Passkeys[keyIndex].LastUsedAt = now.Format(time.RFC3339)
	state.PasskeyAuthenticationChallenge = passkeyChallenge{}
	state.Sessions = compactSessions(state.Sessions, now)
	state.Sessions = append(state.Sessions, authSession{
		TokenSHA256: tokenHash(session),
		Role:        "operator",
		CreatedAt:   now.Format(time.RFC3339),
		ExpiresAt:   now.Add(operatorSessionTTL).Format(time.RFC3339),
		RemoteAddr:  clientIP(r),
		UserAgent:   r.UserAgent(),
	})
	state.UpdatedAt = now.Format(time.RFC3339)
	if err := saveAuthState(root, state); err != nil {
		return "", err
	}
	return session, nil
}

func verifyPasskeyChallenge(challenge passkeyChallenge, clientData webAuthnClientData, expectedType, origin string) error {
	if challenge.Challenge == "" {
		return fmt.Errorf("passkey challenge not found")
	}
	expires, err := time.Parse(time.RFC3339, challenge.ExpiresAt)
	if err != nil || time.Now().After(expires) {
		return fmt.Errorf("passkey challenge expired")
	}
	if clientData.Type != expectedType {
		return fmt.Errorf("unexpected WebAuthn type")
	}
	if subtle.ConstantTimeCompare([]byte(clientData.Challenge), []byte(challenge.Challenge)) != 1 {
		return fmt.Errorf("passkey challenge mismatch")
	}
	if clientData.Origin != origin {
		return fmt.Errorf("passkey origin mismatch")
	}
	return nil
}

func base64URLDecode(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty base64url value")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err == nil {
		return data, nil
	}
	return base64.URLEncoding.DecodeString(raw)
}

func webAuthnRPID(r *http.Request) string {
	host, _ := splitHostPortLoose(r.Host)
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "" {
		return "localhost"
	}
	return host
}

func authenticatorDataFromAttestation(attestationObject []byte) ([]byte, error) {
	value, _, err := parseCBOR(attestationObject)
	if err != nil {
		return nil, fmt.Errorf("invalid attestation CBOR")
	}
	m, ok := value.(map[any]any)
	if !ok {
		return nil, fmt.Errorf("attestation object is not a map")
	}
	authData, ok := cborMapBytes(m, "authData")
	if !ok || len(authData) == 0 {
		return nil, fmt.Errorf("attestation missing authData")
	}
	return authData, nil
}

func parseRegistrationAuthData(authData []byte, rpID string) (parsedRegistrationAuthData, error) {
	var parsed parsedRegistrationAuthData
	if len(authData) < 55 {
		return parsed, fmt.Errorf("authenticator data is too short")
	}
	if err := verifyAuthDataPrefix(authData, rpID); err != nil {
		return parsed, err
	}
	flags := authData[32]
	if flags&0x01 == 0 {
		return parsed, fmt.Errorf("passkey user presence flag missing")
	}
	if flags&0x40 == 0 {
		return parsed, fmt.Errorf("passkey attested credential data missing")
	}
	parsed.SignCount = binary.BigEndian.Uint32(authData[33:37])

	offset := 37 + 16
	if len(authData) < offset+2 {
		return parsed, fmt.Errorf("attested credential data is truncated")
	}
	credentialIDLen := int(binary.BigEndian.Uint16(authData[offset : offset+2]))
	offset += 2
	if credentialIDLen <= 0 || len(authData) < offset+credentialIDLen {
		return parsed, fmt.Errorf("credential id is truncated")
	}
	parsed.CredentialID = append([]byte(nil), authData[offset:offset+credentialIDLen]...)
	offset += credentialIDLen
	if offset >= len(authData) {
		return parsed, fmt.Errorf("credential public key missing")
	}
	_, read, err := parseCBOR(authData[offset:])
	if err != nil {
		return parsed, fmt.Errorf("invalid credential public key CBOR")
	}
	parsed.PublicKeyCOSE = append([]byte(nil), authData[offset:offset+read]...)
	_, alg, err := parseCOSEPublicKey(parsed.PublicKeyCOSE)
	if err != nil {
		return parsed, err
	}
	parsed.Alg = alg
	return parsed, nil
}

func verifyAuthenticationData(authData, clientDataJSON, signature []byte, key passkeyCredential, rpID string) (uint32, error) {
	if len(authData) < 37 {
		return 0, fmt.Errorf("authenticator data is too short")
	}
	if err := verifyAuthDataPrefix(authData, rpID); err != nil {
		return 0, err
	}
	flags := authData[32]
	if flags&0x01 == 0 {
		return 0, fmt.Errorf("passkey user presence flag missing")
	}
	signCount := binary.BigEndian.Uint32(authData[33:37])

	coseKey, err := base64URLDecode(key.PublicKey)
	if err != nil {
		return 0, fmt.Errorf("stored passkey public key is invalid")
	}
	publicKey, alg, err := parseCOSEPublicKey(coseKey)
	if err != nil {
		return 0, err
	}
	if alg != -7 {
		return 0, fmt.Errorf("unsupported passkey public key algorithm")
	}
	clientHash := sha256.Sum256(clientDataJSON)
	signedData := make([]byte, 0, len(authData)+len(clientHash))
	signedData = append(signedData, authData...)
	signedData = append(signedData, clientHash[:]...)
	digest := sha256.Sum256(signedData)
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		return 0, fmt.Errorf("passkey signature rejected")
	}
	return signCount, nil
}

func verifyAuthDataPrefix(authData []byte, rpID string) error {
	if len(authData) < 37 {
		return fmt.Errorf("authenticator data is too short")
	}
	rpHash := sha256.Sum256([]byte(rpID))
	if !bytes.Equal(authData[:32], rpHash[:]) {
		return fmt.Errorf("passkey rp id hash mismatch")
	}
	return nil
}

func parseCOSEPublicKey(raw []byte) (*ecdsa.PublicKey, int, error) {
	value, read, err := parseCBOR(raw)
	if err != nil || read != len(raw) {
		return nil, 0, fmt.Errorf("invalid COSE public key")
	}
	m, ok := value.(map[any]any)
	if !ok {
		return nil, 0, fmt.Errorf("COSE public key is not a map")
	}
	keyType, ok := cborMapInt(m, 1)
	if !ok || keyType != 2 {
		return nil, 0, fmt.Errorf("unsupported COSE key type")
	}
	alg, ok := cborMapInt(m, 3)
	if !ok {
		return nil, 0, fmt.Errorf("COSE public key missing algorithm")
	}
	curve, ok := cborMapInt(m, -1)
	if !ok || curve != 1 {
		return nil, 0, fmt.Errorf("unsupported COSE curve")
	}
	xBytes, ok := cborMapBytes(m, int64(-2))
	if !ok {
		return nil, 0, fmt.Errorf("COSE public key missing x coordinate")
	}
	yBytes, ok := cborMapBytes(m, int64(-3))
	if !ok {
		return nil, 0, fmt.Errorf("COSE public key missing y coordinate")
	}
	if len(xBytes) != 32 || len(yBytes) != 32 {
		return nil, 0, fmt.Errorf("invalid P-256 coordinate length")
	}
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	curveImpl := elliptic.P256()
	if !curveImpl.IsOnCurve(x, y) {
		return nil, 0, fmt.Errorf("COSE public key point is not on P-256")
	}
	return &ecdsa.PublicKey{Curve: curveImpl, X: x, Y: y}, int(alg), nil
}

func parseCBOR(data []byte) (any, int, error) {
	parser := cborParser{data: data}
	value, err := parser.parse()
	if err != nil {
		return nil, parser.pos, err
	}
	return value, parser.pos, nil
}

func (p *cborParser) parse() (any, error) {
	if p.pos >= len(p.data) {
		return nil, fmt.Errorf("unexpected end of CBOR")
	}
	header := p.data[p.pos]
	p.pos++
	major := header >> 5
	additional := header & 0x1f
	length, err := p.readLen(additional)
	if err != nil {
		return nil, err
	}

	switch major {
	case 0:
		if length > 1<<63-1 {
			return nil, fmt.Errorf("CBOR integer overflow")
		}
		return int64(length), nil
	case 1:
		if length > 1<<63-1 {
			return nil, fmt.Errorf("CBOR integer overflow")
		}
		return -1 - int64(length), nil
	case 2:
		if length > uint64(len(p.data)-p.pos) {
			return nil, fmt.Errorf("CBOR byte string truncated")
		}
		out := append([]byte(nil), p.data[p.pos:p.pos+int(length)]...)
		p.pos += int(length)
		return out, nil
	case 3:
		if length > uint64(len(p.data)-p.pos) {
			return nil, fmt.Errorf("CBOR string truncated")
		}
		out := string(p.data[p.pos : p.pos+int(length)])
		p.pos += int(length)
		return out, nil
	case 4:
		out := make([]any, 0, length)
		for i := uint64(0); i < length; i++ {
			v, err := p.parse()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case 5:
		out := make(map[any]any, length)
		for i := uint64(0); i < length; i++ {
			key, err := p.parse()
			if err != nil {
				return nil, err
			}
			value, err := p.parse()
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil
	case 7:
		switch additional {
		case 20:
			return false, nil
		case 21:
			return true, nil
		case 22, 23:
			return nil, nil
		default:
			return nil, fmt.Errorf("unsupported CBOR simple value")
		}
	default:
		return nil, fmt.Errorf("unsupported CBOR major type %d", major)
	}
}

func (p *cborParser) readLen(additional byte) (uint64, error) {
	switch {
	case additional < 24:
		return uint64(additional), nil
	case additional == 24:
		if p.pos >= len(p.data) {
			return 0, fmt.Errorf("CBOR length truncated")
		}
		value := p.data[p.pos]
		p.pos++
		return uint64(value), nil
	case additional == 25:
		if p.pos+2 > len(p.data) {
			return 0, fmt.Errorf("CBOR length truncated")
		}
		value := binary.BigEndian.Uint16(p.data[p.pos : p.pos+2])
		p.pos += 2
		return uint64(value), nil
	case additional == 26:
		if p.pos+4 > len(p.data) {
			return 0, fmt.Errorf("CBOR length truncated")
		}
		value := binary.BigEndian.Uint32(p.data[p.pos : p.pos+4])
		p.pos += 4
		return uint64(value), nil
	case additional == 27:
		if p.pos+8 > len(p.data) {
			return 0, fmt.Errorf("CBOR length truncated")
		}
		value := binary.BigEndian.Uint64(p.data[p.pos : p.pos+8])
		p.pos += 8
		return value, nil
	default:
		return 0, fmt.Errorf("unsupported CBOR indefinite length")
	}
}

func cborMapBytes(m map[any]any, key any) ([]byte, bool) {
	value, ok := m[key]
	if !ok {
		return nil, false
	}
	data, ok := value.([]byte)
	return data, ok
}

func cborMapInt(m map[any]any, key int64) (int64, bool) {
	value, ok := m[key]
	if !ok {
		return 0, false
	}
	n, ok := value.(int64)
	return n, ok
}
