package ingest

import "os"

func mkShort() (string, error) { return os.MkdirTemp("/tmp", "lg") }

func removeAll(d string) { os.RemoveAll(d) }
