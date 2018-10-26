package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

func startServer() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		start := time.Now()
		writer := statusWriter{w, 0, 0}
		proxy(&writer, r)
		end := time.Now()
		latency := end.Sub(start)
		statusCode := writer.status
		length := writer.length
		user, _, ok := r.BasicAuth()
		if !ok {
			user = "-"
		}
		hostFromTo := fmt.Sprintf("%s -> %s:%v", r.Host, *destHostFlag, *destPortFlag)
		cl := fmt.Sprintf("Content-Length: %d", r.ContentLength)
		ct := fmt.Sprintf("Content-Type: %s", r.Header.Get("Content-Type"))

		url := r.URL.Path
		params := r.Form.Encode()
		if params != "" {
			url = url + "?" + params
		}

		writeToLog(fmt.Sprintf("v%s: %s, %s, %s, %s, %s, %s, %s, %s, %d, %d, %d, %s, %s, %v\r\n",
			version,
			r.RemoteAddr,
			user,
			end.Format("2006.01.02"),
			end.Format("15:04:05.000000000"),
			r.Proto,
			hostFromTo,
			ct,
			cl,
			length,
			time.Since(start)/time.Millisecond,
			statusCode,
			r.Method,
			url,
			latency,
		))
	})

	if err := http.ListenAndServe(fmt.Sprintf(":%v", *listenPortFlag), nil); err != nil {
		logError(err)
	}
}

func stopServer() {

}

func proxy(w http.ResponseWriter, r *http.Request) {
	var (
		proto      string
		protoMajor int
		protoMinor int
		ok         bool
	)

	if protoMajor, protoMinor, ok = http.ParseHTTPVersion(*destHostProto); !ok {
		fmt.Fprintf(w, "Malformed HTTP version %s", *destHostProto)
		if *debugFlag {
			fmt.Println("Malformed HTTP version " + *destHostProto)
		}
		return
	}

	if protoMajor == 1 && protoMinor == 0 {
		proto = "http"
	} else if protoMajor == 1 && protoMinor == 1 {
		proto = "https"
	}

	newURL := fmt.Sprintf("%s://%s", proto, *destHostFlag)

	if *destPortFlag != 443 {
		newURL = newURL + fmt.Sprintf(":%v", *destPortFlag)
	}
	newURL = newURL + r.URL.Path
	if r.URL.RawQuery != "" {
		newURL = newURL + "?" + r.URL.RawQuery
	}

	fmt.Println(newURL)
	var (
		err     error
		newReq  *http.Request
		newResp *http.Response
		cert    tls.Certificate
	)

	body, err := ioutil.ReadAll(r.Body)
	if !isError(w, err) {
		return
	}

	newReq, err = http.NewRequest(r.Method, newURL, bytes.NewBuffer(body))

	if !isError(w, err) {
		return
	}

	copyHeaders(newReq.Header, r.Header)

	tlsClientConfig := tls.Config{InsecureSkipVerify: true}

	if (*destCertFlag) != "" {
		cert, err = loadX509KeyPair(*destCertFlag, *destKeyFlag, *destKeyPassFlag)
		if !isError(w, err) {
			return
		}
		tlsClientConfig.Certificates = []tls.Certificate{cert}
	}

	tr := &http.Transport{TLSClientConfig: &tlsClientConfig}

	client := &http.Client{Transport: tr}

	reqDumped := dumpRequest(newReq)
	newResp, err = client.Do(newReq)

	if !isError(w, err) {
		return
	}

	dumpResponse(reqDumped, newResp)

	if !isError(w, err) {
		return
	}

	copyHeaders(w.Header(), newResp.Header)
	w.WriteHeader(newResp.StatusCode)
	io.Copy(w, newResp.Body)
	newResp.Body.Close()
}

func loadX509KeyPair(certFile, keyFile, pw string) (cert tls.Certificate, err error) {
	certPEMBlock, err := ioutil.ReadFile(certFile)
	if err != nil {
		return
	}
	keyPEMBlock, err := ioutil.ReadFile(keyFile)
	if err != nil {
		return
	}
	return x509KeyPair(certPEMBlock, keyPEMBlock, []byte(pw))
}

func x509KeyPair(certPEMBlock, keyPEMBlock, pw []byte) (cert tls.Certificate, err error) {
	var certDERBlock *pem.Block
	for {
		certDERBlock, certPEMBlock = pem.Decode(certPEMBlock)
		if certDERBlock == nil {
			break
		}
		if certDERBlock.Type == "CERTIFICATE" {
			cert.Certificate = append(cert.Certificate, certDERBlock.Bytes)
		}
	}

	if len(cert.Certificate) == 0 {
		err = errors.New("crypto/tls: failed to parse certificate PEM data")
		return
	}
	var keyDERBlock *pem.Block
	for {
		keyDERBlock, keyPEMBlock = pem.Decode(keyPEMBlock)
		if keyDERBlock == nil {
			err = errors.New("crypto/tls: failed to parse key PEM data")
			return
		}
		if x509.IsEncryptedPEMBlock(keyDERBlock) {
			out, err2 := x509.DecryptPEMBlock(keyDERBlock, pw)
			if err2 != nil {
				err = err2
				return
			}
			keyDERBlock.Bytes = out
			break
		}
		if keyDERBlock.Type == "PRIVATE KEY" || strings.HasSuffix(keyDERBlock.Type, " PRIVATE KEY") {
			break
		}
	}

	cert.PrivateKey, err = parsePrivateKey(keyDERBlock.Bytes)
	if err != nil {
		return
	}
	// We don't need to parse the public key for TLS, but we so do anyway
	// to check that it looks sane and matches the private key.
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return
	}

	switch pub := x509Cert.PublicKey.(type) {
	case *rsa.PublicKey:
		priv, ok := cert.PrivateKey.(*rsa.PrivateKey)
		if !ok {
			err = errors.New("crypto/tls: private key type does not match public key type")
			return
		}
		if pub.N.Cmp(priv.N) != 0 {
			err = errors.New("crypto/tls: private key does not match public key")
			return
		}
	case *ecdsa.PublicKey:
		priv, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
		if !ok {
			err = errors.New("crypto/tls: private key type does not match public key type")
			return

		}
		if pub.X.Cmp(priv.X) != 0 || pub.Y.Cmp(priv.Y) != 0 {
			err = errors.New("crypto/tls: private key does not match public key")
			return
		}
	default:
		err = errors.New("crypto/tls: unknown public key algorithm")
		return
	}
	return
}

// Attempt to parse the given private key DER block. OpenSSL 0.9.8 generates
// PKCS#1 private keys by default, while OpenSSL 1.0.0 generates PKCS#8 keys.
// OpenSSL ecparam generates SEC1 EC private keys for ECDSA. We try all three.
func parsePrivateKey(der []byte) (crypto.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		switch key := key.(type) {
		case *rsa.PrivateKey, *ecdsa.PrivateKey:
			return key, nil
		default:
			return nil, errors.New("crypto/tls: found unknown private key type in PKCS#8 wrapping")
		}
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}

	return nil, errors.New("crypto/tls: failed to parse private key")
}

func copyHeaders(dst, src http.Header) {
	for k := range dst {
		dst.Del(k)
	}
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func isError(w http.ResponseWriter, e error) bool {
	if e == nil {
		return true
	}

	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, "Error: %s!", e.Error())
	if *debugFlag {
		fmt.Println(e.Error())
	}
	return false
}
