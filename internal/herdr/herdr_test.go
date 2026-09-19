package herdr

import (
	"reflect"
	"testing"
)

func TestRemoteArgs(t *testing.T) {
	got := remoteArgs("workbox")
	want := []string{"--remote", "workbox"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("remoteArgs = %v, want %v", got, want)
	}
}
