package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spacemonkeygo/openssl"
	"github.com/urfave/cli"
)

type PEMType string

const (
	Cert PEMType = "cert"
	PKey PEMType = "pkey"
)

const (
	PubHeader    = "-----BEGIN CERTIFICATE-----"
	PKeyHeader   = "-----BEGIN PRIVATE KEY-----"
	ConfigHeader = "# ~~~ Autogenerated config start - Do not touch! ~~~"
	ConfigFooter = "# ~~~ Autogenerated config end ~~~"
)

type PublicKey struct {
	path    string
	block   []byte
	cert    *openssl.Certificate
	keyType PEMType
}

type KeyPair struct {
	cert     *openssl.Certificate
	certPath string
	keyPath  string
}

type PublicKeyResult struct {
	res PublicKey
	err error
}

type KeyPairResult struct {
	res KeyPair
	err error
}

func findFiles(base string, files *[]string) error {
	log.Println("Searching for certificates in " + base + "...")

	items, err := ioutil.ReadDir(base)
	if err != nil {
		return err
	}

	for _, file := range items {
		filePath := path.Join(base, file.Name())

		if file.IsDir() {
			findFiles(filePath, files)
		} else {
			*files = append(*files, filePath)
		}
	}

	return nil
}

func getCertAndPubKeyFromCert(content []byte) ([]byte, *openssl.Certificate, error) {
	cert, err := openssl.LoadCertificateFromPEM(content)
	if err != nil {
		return nil, nil, err
	}

	block, _ := pem.Decode(content)

	x509cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}

	if x509cert.NotAfter.Before(time.Now()) {
		return nil, nil, errors.New("expired")
	}

	if err != nil {
		return nil, nil, err
	}

	pubKey, err := cert.PublicKey()
	if err != nil {
		return nil, nil, err
	}

	pubPem, err := pubKey.MarshalPKIXPublicKeyPEM()
	if err != nil {
		return nil, nil, err
	}

	return pubPem, cert, nil
}

func getPubKeyFromPKey(content []byte) ([]byte, error) {
	pkey, err := openssl.LoadPrivateKeyFromPEM(content)
	if err != nil {
		return nil, err
	}

	pubPem, err := pkey.MarshalPKIXPublicKeyPEM()
	if err != nil {
		return nil, err
	}

	return pubPem, nil
}

func loadPEMFile(path string, c chan PublicKeyResult) {
	var pubKey PublicKey

	file, err := os.Open(path)
	if err != nil {
		log.Println("ERROR: Could not open " + path)
		c <- PublicKeyResult{res: pubKey, err: err}
		return
	}

	defer file.Close()

	content, err := ioutil.ReadAll(file)
	if err != nil {
		log.Println("ERROR: Could not read file " + path)
		c <- PublicKeyResult{res: pubKey, err: err}
		return
	}

	var pubKeyPEMBlock []byte
	var cert *openssl.Certificate
	var keyType PEMType = Cert

	if bytes.Contains(content, []byte(PubHeader)) {
		pubKeyPEMBlock, cert, err = getCertAndPubKeyFromCert(content)

		if err == nil {
			log.Println("Certificate: " + path)
		} else if err.Error() == "expired" {
			log.Println("WARNING: Found expored certificate: " + path)
		}
	} else if bytes.Contains(content, []byte(PKeyHeader)) {
		pubKeyPEMBlock, err = getPubKeyFromPKey(content)
		keyType = PKey

		log.Println("Private key: " + path)
	} else {
		c <- PublicKeyResult{res: pubKey, err: errors.New("invalid file")}
		return
	}

	if err != nil {
		log.Println("Could not load public key from cert or private key!")
		c <- PublicKeyResult{res: pubKey, err: err}
		return
	}

	c <- PublicKeyResult{
		res: PublicKey{
			block:   pubKeyPEMBlock,
			path:    path,
			cert:    cert,
			keyType: keyType,
		},
		err: nil,
	}
}

func comparePrivateKeyToCert(publicKey PublicKey, privateKeys *[]PublicKey, c chan KeyPairResult) {
	var keyPair KeyPair

	for _, privateKey := range *privateKeys {
		if bytes.Compare(publicKey.block, privateKey.block) == 0 {
			certPath := publicKey.path
			keyPath := privateKey.path

			log.Println("Valid pair: " + filepath.Base(publicKey.path) + " + " + filepath.Base(privateKey.path))

			c <- KeyPairResult{
				res: KeyPair{
					cert:     publicKey.cert,
					certPath: certPath,
					keyPath:  keyPath,
				},
				err: nil,
			}

			return
		}
	}

	c <- KeyPairResult{res: keyPair, err: errors.New("no match found")}
}

func checkPairs(public *[]PublicKey, private *[]PublicKey) []KeyPair {
	var pairs []KeyPair

	c := make(chan KeyPairResult)

	for _, pub := range *public {
		go comparePrivateKeyToCert(pub, private, c)
	}

	for i := 0; i < len(*public); i++ {
		if keyPairResult := <-c; keyPairResult.err == nil {
			pairs = append(pairs, keyPairResult.res)
		}
	}

	return pairs
}

func getValidCerts(files []string) []KeyPair {
	var public []PublicKey
	var private []PublicKey

	c := make(chan PublicKeyResult)

	for _, path := range files {
		go loadPEMFile(path, c)
	}

	for i := 0; i < len(files); i++ {
		if pubKeyResult := <-c; pubKeyResult.err == nil {
			if pubKeyResult.res.keyType == Cert {
				public = append(public, pubKeyResult.res)
			} else {
				private = append(private, pubKeyResult.res)
			}
		}
	}

	log.Println("Found " + strconv.Itoa(len(public)) + " certificates and " + strconv.Itoa(len(private)) + " private keys!")

	if len(public) == 0 && len(private) == 0 {
		os.Exit(0)
	}

	return checkPairs(&public, &private)
}

func writeTraefikConfigFile(pairs []KeyPair, outFile string, pathPrefix string) {
	log.Println("Found " + strconv.Itoa(len(pairs)) + " valid keypairs!")
	log.Println("Writing config to " + outFile + "...")

	buf := &bytes.Buffer{}

	buf.Write([]byte(ConfigHeader + "\n\n"))

	for _, pair := range pairs {
		certPath := filepath.Join(pathPrefix, pair.certPath)
		keyPath := filepath.Join(pathPrefix, pair.keyPath)

		buf.Write([]byte("[[tls]]\n"))
		buf.Write([]byte("  entryPoints = [\"https\"]\n"))
		buf.Write([]byte("  [tls.certificate]\n"))
		buf.Write([]byte("    certFile = \"" + certPath + "\"\n"))
		buf.Write([]byte("    keyFile = \"" + keyPath + "\"\n"))
		buf.Write([]byte("\n"))
	}

	buf.Write([]byte(ConfigFooter))

	err := ioutil.WriteFile(outFile, buf.Bytes(), 0644)
	if err != nil {
		log.Fatal(err)
	}
}

func run(c *cli.Context) {
	if !c.IsSet("out") {
		log.Fatal("Output file not set!")
	}

	if len(c.Args()) == 0 {
		log.Fatal("Insufficient arguments!")
	}

	var files []string

	base := filepath.Join(c.Args()[0], ".")

	err := findFiles(base, &files)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Found a total of " + strconv.Itoa(len(files)) + " files!")
	log.Println("Searching for certificates and private keys...")

	pairs := getValidCerts(files)
	writeTraefikConfigFile(pairs, c.String("out"), c.String("path-prefix"))
}

func main() {
	app := cli.NewApp()
	app.Name = "traefik-tls-config-gen"
	app.HideVersion = true
	app.Usage = "Generator for traefik TLS certificate config"
	app.UsageText = filepath.Base(os.Args[0]) + " [global options] [certificate directory path]"
	app.Author = "ChrisXF <info@sethorax.com>"

	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "out, o",
			Usage: "Path of generated config file",
		},
		cli.StringFlag{
			Name: "path-prefix, p",
			Usage: "Path prefix for cert and key file paths in config file",
		},
	}

	app.Action = run

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
