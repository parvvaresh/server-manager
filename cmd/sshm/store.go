package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Server is one entry of the vault.
type Server struct {
	Name string
	Host string
	Port string
	User string
	Pass string
	Key  string
	Tags string
	Note string
}

// Target is what ssh gets as its destination: user@host, or host alone.
func (s Server) Target() string {
	if s.User != "" {
		return s.User + "@" + s.Host
	}
	return s.Host
}

// Label is Target with the port appended when it is not the default one.
func (s Server) Label() string {
	if s.Port != "" && s.Port != "22" {
		return s.Target() + ":" + s.Port
	}
	return s.Target()
}

// Store holds the vault: the servers plus where and how they are persisted.
type Store struct {
	Home   string
	Plain  string
	Enc    string
	Master string // set once the encrypted vault has been unlocked
	List   []Server
}

func NewStore() *Store {
	home := os.Getenv("SSHM_HOME")
	if home == "" {
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(userHome(), ".config")
		}
		home = filepath.Join(cfg, "sshm")
	}
	return &Store{
		Home:  home,
		Plain: filepath.Join(home, "servers.tsv"),
		Enc:   filepath.Join(home, "servers.enc"),
	}
}

func userHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func (st *Store) EnsureHome() error {
	if err := os.MkdirAll(st.Home, 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", st.Home, err)
	}
	_ = os.Chmod(st.Home, 0o700)
	return nil
}

func (st *Store) Encrypted() bool {
	fi, err := os.Stat(st.Enc)
	return err == nil && !fi.IsDir()
}

// Unlock asks for the master password (up to three tries) and keeps it, so
// that a later Save can re-encrypt without asking again.
func (st *Store) Unlock() error {
	if !st.Encrypted() || st.Master != "" {
		return nil
	}
	blob, err := os.ReadFile(st.Enc)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", st.Enc, err)
	}
	for tries := 0; tries < 3; tries++ {
		pass, err := readSecret("Master password: ")
		if err != nil {
			return err
		}
		if _, err := decryptVault(blob, pass); err == nil {
			st.Master = pass
			return nil
		}
		warn("wrong password")
	}
	return errors.New("could not unlock the vault")
}

// Load reads the vault from disk, decrypting it first when needed.
func (st *Store) Load() error {
	st.List = nil
	if st.Encrypted() {
		if err := st.Unlock(); err != nil {
			return err
		}
		blob, err := os.ReadFile(st.Enc)
		if err != nil {
			return err
		}
		plain, err := decryptVault(blob, st.Master)
		if err != nil {
			return err
		}
		st.List = parseTSV(plain)
		return nil
	}
	data, err := os.ReadFile(st.Plain)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	st.List = parseTSV(data)
	return nil
}

// Save writes the vault back, keeping its current encrypted/plain shape.
func (st *Store) Save() error {
	if err := st.EnsureHome(); err != nil {
		return err
	}
	text := st.TSV(true)
	if st.Encrypted() {
		if err := st.Unlock(); err != nil {
			return err
		}
		blob, err := encryptVault(text, st.Master)
		if err != nil {
			return fmt.Errorf("encryption failed: %w", err)
		}
		return writeFileAtomic(st.Enc, blob)
	}
	return writeFileAtomic(st.Plain, text)
}

// TSV serialises the servers, one tab separated record per line.
func (st *Store) TSV(withPasswords bool) []byte {
	var b bytes.Buffer
	for _, s := range st.List {
		pass := s.Pass
		if !withPasswords {
			pass = ""
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Host, s.Port, s.User, pass, s.Key, s.Tags, s.Note)
	}
	return b.Bytes()
}

// Find returns the index of a server by name (case-insensitive), or -1.
func (st *Store) Find(name string) int {
	want := strings.ToLower(name)
	for i, s := range st.List {
		if strings.ToLower(s.Name) == want {
			return i
		}
	}
	return -1
}

func (st *Store) Delete(i int) {
	if i < 0 || i >= len(st.List) {
		return
	}
	st.List = append(st.List[:i], st.List[i+1:]...)
}

// Put inserts a server, or replaces the one at index i when i >= 0.
func (st *Store) Put(i int, s Server) {
	if s.Port == "" {
		s.Port = "22"
	}
	if i >= 0 && i < len(st.List) {
		st.List[i] = s
		return
	}
	st.List = append(st.List, s)
}

func parseTSV(data []byte) []Server {
	var out []Server
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		for len(f) < 8 {
			f = append(f, "")
		}
		s := Server{
			Name: f[0], Host: f[1], Port: f[2], User: f[3],
			Pass: f[4], Key: f[5], Tags: f[6], Note: f[7],
		}
		if s.Name == "" {
			continue
		}
		if s.Port == "" {
			s.Port = "22"
		}
		out = append(out, s)
	}
	return out
}

// clean strips the characters that would break the one-record-per-line format.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ------------------------------------------------------------ encryption --

// The on-disk format is exactly what `openssl enc -aes-256-cbc -pbkdf2
// -iter 200000 -salt` produces, so vaults stay readable by both the shell
// version of sshm and the openssl CLI.

const (
	vaultMagic = "Salted__"
	vaultIter  = 200000
)

var errWrongPassword = errors.New("wrong password")

func encryptVault(plain []byte, pass string) ([]byte, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, iv := deriveKeyIV(pass, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plain, block.BlockSize())
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	out := make([]byte, 0, len(vaultMagic)+len(salt)+len(ct))
	out = append(out, vaultMagic...)
	out = append(out, salt...)
	out = append(out, ct...)
	return out, nil
}

func decryptVault(blob []byte, pass string) ([]byte, error) {
	if len(blob) < 16 || string(blob[:8]) != vaultMagic {
		return nil, errors.New("not an sshm vault (bad header)")
	}
	salt := blob[8:16]
	ct := blob[16:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("corrupt vault (bad length)")
	}
	key, iv := deriveKeyIV(pass, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	plain, err = pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		return nil, errWrongPassword
	}
	// A vault is text; anything else means the key was wrong and the padding
	// check happened to pass.
	if !utf8.Valid(plain) {
		return nil, errWrongPassword
	}
	return plain, nil
}

// deriveKeyIV mirrors OpenSSL's PBKDF2 key derivation for aes-256-cbc:
// 32 bytes of key followed by 16 bytes of IV, HMAC-SHA256, salted.
func deriveKeyIV(pass string, salt []byte) (key, iv []byte) {
	dk := pbkdf2SHA256([]byte(pass), salt, vaultIter, 48)
	return dk[:32], dk[32:48]
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	dk := make([]byte, 0, blocks*hashLen)
	u := make([]byte, 0, hashLen)
	t := make([]byte, hashLen)
	var counter [4]byte

	for b := 1; b <= blocks; b++ {
		counter[0] = byte(b >> 24)
		counter[1] = byte(b >> 16)
		counter[2] = byte(b >> 8)
		counter[3] = byte(b)

		prf.Reset()
		prf.Write(salt)
		prf.Write(counter[:])
		u = prf.Sum(u[:0])
		copy(t, u)

		for i := 2; i <= iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(append([]byte{}, b...), bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, errors.New("bad padding")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > size || n > len(b) {
		return nil, errors.New("bad padding")
	}
	pad := b[len(b)-n:]
	if subtle.ConstantTimeCompare(pad, bytes.Repeat([]byte{byte(n)}, n)) != 1 {
		return nil, errors.New("bad padding")
	}
	return b[:len(b)-n], nil
}
