package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"dead-drop/lib"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/awnumar/memguard"
	"github.com/mitchellh/go-homedir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

const remoteFlag = "remote"
const privKeyFlag = "private-key"
const encryptionKeyFlag = "encryption-key"
const keyNameFlag = "key-name"
const insecureSkipVerifyFlag = "insecure-skip-verify"

var confFile string
var keyNameRegex = regexp.MustCompile(lib.KeyNameRegex)

func main() {
	cobra.OnInitialize(loadConfig)

	var rootCmd = &cobra.Command{Use: "dead"}
	rootCmd.AddCommand(setupDropCmd(), setupPullCmd(), setupAddKeyCmd(), setupKeyGenCmd())

	rootCmd.PersistentFlags().StringVar(&confFile, "config", "",
		"config file (default is "+filepath.Join("$HOME", lib.DefaultConfigDir, lib.DefaultConfigName)+".yml)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Printf("FATAL: Failed to execute command: %v\n", err)
		os.Exit(1)
	}
}

func loadConfig() {
	if confFile != "" {
		viper.SetConfigFile(confFile)
	} else {
		viper.AddConfigPath(filepath.Join("$HOME", lib.DefaultConfigDir))
		viper.SetConfigName(lib.DefaultConfigName)
		viper.SetConfigType(lib.DefaultConfigType)
	}

	if err := viper.ReadInConfig(); err != nil {
		fmt.Printf("Error reading config file: %v\n", err)
		os.Exit(1)
	}
}

func getStringFlag(flag string) (string, error) {
	value := viper.GetString(flag)
	if value == "" {
		return "", fmt.Errorf("flag '%s' not specified or empty", flag)
	}

	return value, nil
}

func bindPFlag(cmd *cobra.Command, flag string) {
	if err := viper.BindPFlag(flag, cmd.PersistentFlags().Lookup(flag)); err != nil {
		fmt.Printf("Error binding %s flag for the %s command: %v\n", flag, cmd.Name(), err)
	}
}

func setupEncryptionFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().String(encryptionKeyFlag, "", "Encryption key")
}

func bindEncryptionFlags(cmd *cobra.Command) {
	bindPFlag(cmd, encryptionKeyFlag)
}

func setupRemoteCmdFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().String(remoteFlag, "", "Remote dead-drop host")
	cmd.PersistentFlags().String(privKeyFlag, "",
		"Private key to use for authentication (e.g. generated by keygen)")
	cmd.PersistentFlags().String(keyNameFlag, "", "Key name to use for authentication")
	cmd.PersistentFlags().Bool(insecureSkipVerifyFlag, false, "Skip tls certificate verification")
}

func bindRemoteCmdFlags(cmd *cobra.Command) {
	bindPFlag(cmd, remoteFlag)
	bindPFlag(cmd, privKeyFlag)
	bindPFlag(cmd, keyNameFlag)
	bindPFlag(cmd, insecureSkipVerifyFlag)

	insecureSkipVerify := viper.GetBool(insecureSkipVerifyFlag)
	if insecureSkipVerify {
		fmt.Printf("WARN: Skipping tls certificate verification, be careful!\n")
	}
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: insecureSkipVerify}
}

func setupDropCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drop <file path>",
		Short: "Drop a file to remote",
		Args:  cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			filePath := args[0]

			bindRemoteCmdFlags(cmd)
			bindEncryptionFlags(cmd)

			or, err := drop(filePath)
			if err != nil {
				fmt.Printf("ERROR: Failed to drop file '%s': %v\n", filePath, err)
				os.Exit(1)
			}

			fmt.Printf("Dropped %s -> %s\n", filePath, or)
		},
	}

	setupRemoteCmdFlags(cmd)
	setupEncryptionFlags(cmd)

	return cmd
}

func setupPullCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pull <object> <destination path>",
		Short: "Pull a dropped object from remote",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			object := args[0]
			destPath := args[1]

			bindRemoteCmdFlags(cmd)
			bindEncryptionFlags(cmd)

			if err := pull(object, destPath); err != nil {
				fmt.Printf("ERROR: Failed to pull object '%s': %v\n", object, err)
				os.Exit(1)
			}

			fmt.Printf("Pulled %s <- %s\n", destPath, object)
		},
	}

	setupRemoteCmdFlags(cmd)
	setupEncryptionFlags(cmd)

	return cmd
}

func setupAddKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-key <public key path> <key name>",
		Short: "Add a public key as an authorized key on remote",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			pubKeyPath := args[0]
			keyName := args[1]

			bindRemoteCmdFlags(cmd)

			if err := addKey(pubKeyPath, keyName); err != nil {
				fmt.Printf("ERROR: Failed to add authorized key '%s': %v\n", pubKeyPath, err)
				os.Exit(1)
			}

			fmt.Printf("Added %s -> %s\n", pubKeyPath, keyName)
		},
	}

	setupRemoteCmdFlags(cmd)

	return cmd
}

func setupKeyGenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gen-key <private key path> <public key path>",
		Short: "Generates an RSA key-pair, for use authenticating requests",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			privPath := args[0]
			pubPath := args[1]

			if err := keyGen(privPath, pubPath); err != nil {
				fmt.Printf("ERROR: Failed to generate key-pair: %v\n", err)
				os.Exit(1)
			}
		},
	}
}

func checksum(data []byte) string {
	checksumBytes := sha256.Sum256(data)
	return base64.URLEncoding.EncodeToString(checksumBytes[:])
}

func loadEncryptionKey(rawPath string) (*memguard.LockedBuffer, error) {
	encryptionKeyPath, err := homedir.Expand(rawPath)
	if err != nil {
		return nil, fmt.Errorf("error locating encryption key: %v", err)
	}

	encryptionKeyReader, err := os.Open(encryptionKeyPath)
	if err != nil {
		return nil, fmt.Errorf("error reading encryption key '%s': %v", encryptionKeyPath, err)
	}
	encryptionKey := memguard.NewBufferFromEntireReader(encryptionKeyReader)

	return encryptionKey, nil
}

// TODO(shane) this function is quite long, try to split it up.
func drop(filePath string) (*ObjectReference, error) {
	remote, err := getStringFlag(remoteFlag)
	if err != nil {
		return nil, err
	}

	encryptionKeyRawPath, err := getStringFlag(encryptionKeyFlag)
	if err != nil {
		return nil, err
	}

	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file '%s': %v", filePath, err)
	}

	fmt.Printf("Encrypting object with AES-CTR + HMAC-SHA-265 ...\n")

	encryptionKey, err := loadEncryptionKey(encryptionKeyRawPath)
	if err != nil {
		return nil, err
	}

	data, err = encrypt(encryptionKey, data)
	if err != nil {
		return nil, fmt.Errorf("error encrypting object: %v", err)
	}

	remoteUrl := fmt.Sprintf("%s/d", remote)

	client := &http.Client{}

	req, err := http.NewRequest("POST", remoteUrl, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("error building request: %v", err)
	}

	req.Header.Set("Content-Type", "application/octet-stream")

	fmt.Printf("Uploading object ...\n")

	resp, err := makeAuthenticatedRequest(client, req, remote)
	if err != nil {
		return nil, err
	}

	oid, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %v", err)
	}

	or := &ObjectReference{
		oid:      string(oid),
		checksum: checksum(data),
	}
	return or, nil
}

// TODO(shane) this function is quite long, try to split it up.
func pull(object string, destPath string) error {
	or, err := parseObjectReference(object)
	if err != nil {
		return err
	}

	remote, err := getStringFlag(remoteFlag)
	if err != nil {
		return err
	}

	encryptionKeyRawPath, err := getStringFlag(encryptionKeyFlag)
	if err != nil {
		return err
	}

	remoteUrl := fmt.Sprintf("%s/d/%s", remote, or.oid)

	client := &http.Client{}

	req, err := http.NewRequest("GET", remoteUrl, nil)
	if err != nil {
		return fmt.Errorf("error building request: %v", err)
	}

	fmt.Printf("Downloading object ...\n")

	resp, err := makeAuthenticatedRequest(client, req, remote)
	if err != nil {
		return err
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response body: %v", err)
	}

	fmt.Printf("Verifying checksum ...\n")
	if checksum(data) != or.checksum {
		return fmt.Errorf("object integrity compromised, discarding unsafe pull")
	}

	fmt.Printf("Decrypting object with AES-CTR + HMAC-SHA-265 ...\n")

	encryptionKey, err := loadEncryptionKey(encryptionKeyRawPath)
	if err != nil {
		return err
	}

	dataBuf, err := decrypt(encryptionKey, data)
	if err != nil {
		return fmt.Errorf("error decrypting object: %v", err)
	}
	defer dataBuf.Destroy()
	data = dataBuf.Bytes()

	if err = ioutil.WriteFile(destPath, data, lib.ObjectPerms); err != nil {
		return fmt.Errorf("error writing object to '%s': %v", destPath, err)
	}

	return nil
}

func addKey(pubKeyPath string, keyName string) error {
	remote, err := getStringFlag(remoteFlag)
	if err != nil {
		return err
	}

	remoteUrl := fmt.Sprintf("%s/add-key", remote)

	client := &http.Client{}

	pubKeyBytes, err := ioutil.ReadFile(pubKeyPath)
	if err != nil {
		return fmt.Errorf("error reading public key '%s': %v", pubKeyPath, err)
	}

	payload := lib.AddKeyPayload{
		Key:     pubKeyBytes,
		KeyName: keyName,
	}

	body := new(bytes.Buffer)
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return err
	}

	req, err := http.NewRequest("POST", remoteUrl, body)
	if err != nil {
		return fmt.Errorf("error building request: %v", err)
	}

	_, err = makeAuthenticatedRequest(client, req, remote)
	return err
}

func keyGen(privPath string, pubPath string) error {
	privKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("failed generating private key: %v", err)
	}

	privKeyBytes := pem.EncodeToMemory(&pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   x509.MarshalPKCS1PrivateKey(privKey),
	})

	if err := ioutil.WriteFile(privPath, privKeyBytes, lib.PrivateKeyPerms); err != nil {
		return fmt.Errorf("failed to write private key: %v", err)
	}
	fmt.Printf("Wrote private key to %s\n", privPath)

	pubKeyBytes := pem.EncodeToMemory(&pem.Block{
		Type:    "RSA PUBLIC KEY",
		Headers: nil,
		Bytes:   x509.MarshalPKCS1PublicKey(&privKey.PublicKey),
	})

	if err := ioutil.WriteFile(pubPath, pubKeyBytes, lib.PublicKeyPerms); err != nil {
		return fmt.Errorf("failed to write public key: %v", err)
	}
	fmt.Printf("Wrote public key to %s\n", pubPath)

	return nil
}

func makeAuthenticatedRequest(client *http.Client, req *http.Request, remote string) (*http.Response, error) {
	resp, err := makeAuthenticatedRequestInternal(client, req, remote)
	if err != nil {
		return resp, fmt.Errorf("request failed: %v", err)
	}
	if resp.StatusCode != 200 {
		return resp, fmt.Errorf("request failed with status: %s", resp.Status)
	}

	return resp, nil
}

func makeAuthenticatedRequestInternal(client *http.Client, req *http.Request, remote string) (*http.Response, error) {
	keyName, err := getStringFlag(keyNameFlag)
	if err != nil {
		return nil, err
	}

	if !keyNameRegex.Match([]byte(keyName)) {
		return nil, fmt.Errorf("invalid key name")
	}

	for i := 0; true; i++ {
		token, err := authenticate(remote, keyName)
		if err != nil {
			return nil, fmt.Errorf("authentication failed: %v", err)
		}

		req.Header.Set("Authorization", token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && i < 1 {
			// If we get here it is because the JWT secret rotated between the two requests.
			// This happens infrequently, so retrying will succeed.
			continue
		}

		return resp, nil
	}

	// Unreachable.
	return nil, nil
}

func authenticate(remote string, keyName string) (string, error) {
	rawPrivKeyPath, err := getStringFlag(privKeyFlag)
	if err != nil {
		return "", err
	}
	privKeyPath, err := homedir.Expand(rawPrivKeyPath)
	if err != nil {
		return "", fmt.Errorf("error locating private key: %v\n", err)
	}

	remoteUrl := fmt.Sprintf("%s/token", remote)

	payload := lib.TokenRequestPayload{
		KeyName: keyName,
	}

	body := new(bytes.Buffer)
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return "", err
	}

	resp, err := http.Post(remoteUrl, "application/json", body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("response status: %s\n", resp.Status)
	}

	ciphertext, err := ioutil.ReadAll(resp.Body)

	privKeyBytes, err := ioutil.ReadFile(privKeyPath)
	if err != nil {
		return "", fmt.Errorf("error reading private key '%s': %v", privKeyPath, err)
	}

	privKeyDer, _ := pem.Decode(privKeyBytes)
	if privKeyDer == nil {
		return "", fmt.Errorf("failed to decode pem bytes\n")
	}
	privKey, err := x509.ParsePKCS1PrivateKey(privKeyDer.Bytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse private key: %v\n", err)
	}

	token, err := rsa.DecryptOAEP(sha512.New(), rand.Reader, privKey, ciphertext, []byte(lib.TokenCipherLabel))
	if err != nil {
		return "", fmt.Errorf("failed to decrypt authorization token: %v\n", err)
	}

	return string(token), nil
}
