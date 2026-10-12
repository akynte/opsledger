package aggregate

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

func shortDir() (string, error) { return os.MkdirTemp("/tmp", "lg") }

func removeDir(d string) { os.RemoveAll(d) }

func nowRecord() []byte {
	now := time.Now()
	return fmt.Appendf(nil, `<190>Oct 12 12:00:00 h ledger: {"v":1,"t":"%d.%03d","srv":"pub","m":"GET","u":"/","st":200,"rt":"0.002","rsc":"","bot":"0","probe":"0","b":10}`,
		now.Unix(), now.Nanosecond()/1e6)
}

func sendDatagram(t *testing.T, path string, b []byte) {
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	conn.Write(b)
}
