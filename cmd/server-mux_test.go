/*
 * Minio Cloud Storage, (C) 2015, 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestListenerAcceptAfterClose(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for i := 0; i < 10; i++ {
				runTest(t)
			}
		}()
	}
	wg.Wait()
}

func runTest(t *testing.T) {
	const connectionsBeforeClose = 1

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ln = newListenerMux(ln, &tls.Config{})

	addr := ln.Addr().String()
	waitForListener := make(chan error)
	go func() {
		defer close(waitForListener)

		var connCount int
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}

			connCount++
			if connCount > connectionsBeforeClose {
				waitForListener <- errUnexpected
				return
			}
			conn.Close()
		}
	}()

	for i := 0; i < connectionsBeforeClose; i++ {
		err = dial(addr)
		if err != nil {
			t.Fatal(err)
		}
	}

	ln.Close()
	dial(addr)

	err = <-waitForListener
	if err != nil {
		t.Fatal(err)
	}
}

func dial(addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err == nil {
		conn.Close()
	}
	return err
}

// Tests initalizing listeners.
func TestInitListeners(t *testing.T) {
	testCases := []struct {
		serverAddr string
		shouldPass bool
	}{
		// Test 1 with ip and port.
		{
			serverAddr: "127.0.0.1:" + getFreePort(),
			shouldPass: true,
		},
		// Test 2 only port.
		{
			serverAddr: ":" + getFreePort(),
			shouldPass: true,
		},
		// Test 3 with no port error.
		{
			serverAddr: "127.0.0.1",
			shouldPass: false,
		},
		// Test 4 with 'foobar' host not resolvable.
		{
			serverAddr: "foobar:9000",
			shouldPass: false,
		},
	}
	for i, testCase := range testCases {
		listeners, err := initListeners(testCase.serverAddr, &tls.Config{})
		if testCase.shouldPass {
			if err != nil {
				t.Fatalf("Test %d: Unable to initialize listeners %s", i+1, err)
			}
			for _, listener := range listeners {
				if err = listener.Close(); err != nil {
					t.Fatalf("Test %d: Unable to close listeners %s", i+1, err)
				}
			}
		}
		if err == nil && !testCase.shouldPass {
			t.Fatalf("Test %d: Should fail but is successful", i+1)
		}
	}
	// Windows doesn't have 'localhost' hostname.
	if runtime.GOOS != "windows" {
		listeners, err := initListeners("localhost:"+getFreePort(), &tls.Config{})
		if err != nil {
			t.Fatalf("Test 3: Unable to initialize listeners %s", err)
		}
		for _, listener := range listeners {
			if err = listener.Close(); err != nil {
				t.Fatalf("Test 3: Unable to close listeners %s", err)
			}
		}
	}
}

func TestClose(t *testing.T) {
	// Create ServerMux
	m := NewServerMux("", nil)

	if err := m.Close(); err != nil {
		t.Error("Server errored while trying to Close", err)
	}

	// Closing again should return an error.
	if err := m.Close(); err.Error() != "Server has been closed" {
		t.Error("Unexepcted error expected \"Server has been closed\", got", err)
	}
}

func TestServerMux(t *testing.T) {
	ts := httptest.NewUnstartedServer(nil)
	defer ts.Close()

	// Create ServerMux
	m := NewServerMux("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))

	// Set the test server config to the mux
	ts.Config = m.Server
	ts.Start()

	// Create a ListenerMux
	lm := &ListenerMux{
		Listener: ts.Listener,
		config:   &tls.Config{},
		cond:     sync.NewCond(&sync.Mutex{}),
	}
	m.listeners = []*ListenerMux{lm}

	client := http.Client{}
	res, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "hello" {
		t.Errorf("got %q, want hello", string(got))
	}

	// Make sure there is only 1 connection
	m.mu.Lock()
	if len(m.conns) < 1 {
		t.Fatal("Should have 1 connections")
	}
	m.mu.Unlock()

	// Close the server
	m.Close()

	// Make sure there are zero connections
	m.mu.Lock()
	if len(m.conns) > 0 {
		t.Fatal("Should have 0 connections")
	}
	m.mu.Unlock()
}

func TestServerCloseBlocking(t *testing.T) {
	ts := httptest.NewUnstartedServer(nil)
	defer ts.Close()

	// Create ServerMux
	m := NewServerMux("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))

	// Set the test server config to the mux
	ts.Config = m.Server
	ts.Start()

	// Create a ListenerMux.
	lm := &ListenerMux{
		Listener: ts.Listener,
		config:   &tls.Config{},
		cond:     sync.NewCond(&sync.Mutex{}),
	}
	m.listeners = []*ListenerMux{lm}

	dial := func() net.Conn {
		c, cerr := net.Dial("tcp", ts.Listener.Addr().String())
		if cerr != nil {
			t.Fatal(cerr)
		}
		return c
	}

	// Dial to open a StateNew but don't send anything
	cnew := dial()
	defer cnew.Close()

	// Dial another connection but idle after a request to have StateIdle
	cidle := dial()
	defer cidle.Close()
	cidle.Write([]byte("HEAD / HTTP/1.1\r\nHost: foo\r\n\r\n"))
	_, err := http.ReadResponse(bufio.NewReader(cidle), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Make sure we don't block forever.
	m.Close()

	// Make sure there are zero connections
	m.mu.Lock()
	if len(m.conns) > 0 {
		t.Fatal("Should have 0 connections")
	}
	m.mu.Unlock()
}

func TestListenAndServePlain(t *testing.T) {
	wait := make(chan struct{})
	addr := net.JoinHostPort("127.0.0.1", getFreePort())
	errc := make(chan error)
	once := &sync.Once{}

	// Initialize done channel specifically for each tests.
	globalServiceDoneCh = make(chan struct{}, 1)
	// Initialize signal channel specifically for each tests.
	globalServiceSignalCh = make(chan serviceSignal, 1)

	// Create ServerMux and when we receive a request we stop waiting
	m := NewServerMux(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
		once.Do(func() { close(wait) })
	}))

	// ListenAndServe in a goroutine, but we don't know when it's ready
	go func() { errc <- m.ListenAndServe("", "") }()

	wg := &sync.WaitGroup{}
	wg.Add(1)
	// Keep trying the server until it's accepting connections
	go func() {
		client := http.Client{Timeout: time.Millisecond * 10}
		ok := false
		for !ok {
			res, _ := client.Get("http://" + addr)
			if res != nil && res.StatusCode == http.StatusOK {
				ok = true
			}
		}

		wg.Done()
	}()

	wg.Wait()

	// Block until we get an error or wait closed
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-wait:
		m.Close() // Shutdown the ServerMux
		return
	}
}

func TestListenAndServeTLS(t *testing.T) {
	wait := make(chan struct{})
	addr := net.JoinHostPort("127.0.0.1", getFreePort())
	errc := make(chan error)
	once := &sync.Once{}

	// Initialize done channel specifically for each tests.
	globalServiceDoneCh = make(chan struct{}, 1)

	// Create ServerMux and when we receive a request we stop waiting
	m := NewServerMux(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
		once.Do(func() { close(wait) })
	}))

	// Create a cert
	err := createCertsPath()
	if err != nil {
		t.Fatal(err)
	}
	certFile := mustGetCertFile()
	keyFile := mustGetKeyFile()
	defer os.RemoveAll(certFile)
	defer os.RemoveAll(keyFile)

	err = generateTestCert(addr)
	if err != nil {
		t.Error(err)
		return
	}

	// ListenAndServe in a goroutine, but we don't know when it's ready
	go func() { errc <- m.ListenAndServe(certFile, keyFile) }()

	wg := &sync.WaitGroup{}
	wg.Add(1)
	// Keep trying the server until it's accepting connections
	go func() {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client := http.Client{
			Timeout:   time.Millisecond * 10,
			Transport: tr,
		}
		okTLS := false
		for !okTLS {
			res, _ := client.Get("https://" + addr)
			if res != nil && res.StatusCode == http.StatusOK {
				okTLS = true
			}
		}

		okNoTLS := false
		for !okNoTLS {
			res, _ := client.Get("http://" + addr)
			// Without TLS we expect a re-direction from http to https
			// And also the request is not rejected.
			if res != nil && res.StatusCode == http.StatusOK && res.Request.URL.Scheme == "https" {
				okNoTLS = true
			}
		}
		wg.Done()
	}()

	wg.Wait()

	// Block until we get an error or wait closed
	select {
	case err := <-errc:
		if err != nil {
			t.Error(err)
			return
		}
	case <-wait:
		m.Close() // Shutdown the ServerMux
		return
	}
}

// generateTestCert creates a cert and a key used for testing only
func generateTestCert(host string) error {
	certPath := mustGetCertFile()
	keyPath := mustGetKeyFile()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Minio Test Cert"},
		},
		NotBefore: time.Now().UTC(),
		NotAfter:  time.Now().UTC().Add(time.Minute * 1),

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = append(template.IPAddresses, ip)
	}

	template.IsCA = true
	template.KeyUsage |= x509.KeyUsageCertSign

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	certOut, err := os.Create(certPath)
	if err != nil {
		return err
	}
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	certOut.Close()

	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	keyOut.Close()
	return nil
}
