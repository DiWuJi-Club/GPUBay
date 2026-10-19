package api

import "testing"

func TestSSHCommandFormat(t *testing.T) {
	for _, c := range []struct {
		port int
		want string
	}{
		{0, "ssh ins-a@ssh.spark.test"},
		{22, "ssh ins-a@ssh.spark.test"},
		{2222, "ssh -p 2222 ins-a@ssh.spark.test"},
	} {
		s := &Server{SSHHost: "ssh.spark.test", SSHPort: c.port}
		if got := sshCommand("ins-a", s.SSHHost, s.sshPort()); got != c.want {
			t.Errorf("port %d: got %q want %q", c.port, got, c.want)
		}
	}
}
