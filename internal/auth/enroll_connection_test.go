package auth

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEnrollmentKeepsConsumedConnectionReusable(t *testing.T) {
	e, _, _ := setupEnroller(t, "enrollment-secret")
	_, pub := genPubKeyB64(t)
	ts := httptest.NewServer(e)
	defer ts.Close()
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(ts.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(c)
	body := enrollBody(t, "enrollment-secret", pub).String()
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		if _, err := fmt.Fprintf(c, "POST /api/portwing/enroll HTTP/1.1\r\nHost: localhost\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != status || resp.Close {
			t.Fatalf("response = %d, close=%v, read error=%v", resp.StatusCode, resp.Close, readErr)
		}
	}
}
