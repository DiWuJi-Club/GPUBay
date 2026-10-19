package appdeploy_test

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
)

func TestBundleContents(t *testing.T) {
	s := goodSpec()
	if err := appdeploy.ValidateSpec(&s); err != nil {
		t.Fatal(err)
	}
	in := appdeploy.BundleInput{Slug: "vllm", InstanceID: "ins-aaaaaaaa", AppRoot: "/root/data", SpecHash: "abc", Spec: s, Script: "echo hi\n",
		Env: map[string]string{"MODEL_ID": "Qwen/Qwen3-8B"}, Secret: map[string]string{"API_KEY": "abcdefgh"},
		HFEndpoint: "https://hf-mirror.com", PipIndexURL: "https://pypi.tuna.tsinghua.edu.cn/simple"}
	b, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	files := map[string]string{}
	modes := map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		files[h.Name] = string(body)
		modes[h.Name] = h.Mode
	}
	if strings.Join(names, ",") != "run.sh,app.sh,spec.json,env" {
		t.Fatalf("entries = %v", names)
	}
	if modes["run.sh"] != 0o755 || modes["env"] != 0o600 || modes["app.sh"] != 0o644 {
		t.Fatalf("modes = %v", modes)
	}
	env := files["env"]
	for _, line := range []string{
		"export SPARK_APP='vllm'", "export SPARK_APP_DIR='/root/data/.spark-apps/vllm'", "export SPARK_DATA_DIR='/root/data'", "export SPARK_PORT='8000'",
		"export SPARK_PORT_2=''", "export SPARK_INSTANCE_ID='ins-aaaaaaaa'", "export SPARK_MODELS_DIR='/root/data/models'", "export SPARK_SPEC_HASH='abc'",
		"export SPARK_TIMEOUT_S='3600'", "export SPARK_HEALTH_PATH='/health'", "export SPARK_MIN_FREE_GIB='0'", "export SPARK_HEALTH_ADDR='127.0.0.1:8000'",
		"export HF_ENDPOINT='https://hf-mirror.com'", "export MODELSCOPE_CACHE='/root/data/models/modelscope'", "export HF_HOME='/root/data/models/hf'",
		"export PIP_INDEX_URL='https://pypi.tuna.tsinghua.edu.cn/simple'", "export MODEL_ID='Qwen/Qwen3-8B'", "export API_KEY='abcdefgh'",
	} {
		if !strings.Contains(env, line+"\n") {
			t.Errorf("env missing %q:\n%s", line, env)
		}
	}
	if !strings.Contains(files["spec.json"], `"port":8000`) {
		t.Fatalf("spec.json = %s", files["spec.json"])
	}
	// 可重复：同样输入同样字节（mtime 固定）
	b2, _ := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
	if !bytes.Equal(b, b2) {
		t.Fatal("bundle not deterministic")
	}
	for _, bad := range []struct{ env, secret map[string]string }{
		{env: map[string]string{"PATH": "/evil"}},
		{env: map[string]string{"SPARK_APP": "x"}},
		{env: map[string]string{"LD_AUDIT": "x"}},
		{env: map[string]string{"X; touch /tmp/p": "x"}},
		{secret: map[string]string{"bad-name": "abcdefgh"}},
		{secret: map[string]string{"IFS": "abcdefgh"}},
	} {
		badIn := in
		badIn.Env, badIn.Secret = bad.env, bad.secret
		if _, err := appdeploy.Bundle(badIn, []byte("#!/bin/sh\n")); err == nil {
			t.Errorf("bundle accepted bad env name: %v %v", bad.env, bad.secret)
		}
	}
	noPorts := in
	noPorts.Spec.Ports = nil
	if _, err := appdeploy.Bundle(noPorts, []byte("#!/bin/sh\n")); err == nil {
		t.Fatal("spec without ports accepted")
	}
	in.Script = strings.Repeat("x", appdeploy.MaxBundleBytes)
	if _, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n")); err == nil {
		t.Fatal("oversized bundle accepted")
	}
	if got := appdeploy.AppDir("/tmp/x", "open-webui"); got != "/tmp/x/.spark-apps/open-webui" {
		t.Fatalf("AppDir = %q", got)
	}
}

func TestUploadCommand(t *testing.T) {
	got := appdeploy.UploadCommand(appdeploy.AppDir("/root/data", "vllm"))
	want := "mkdir -p '/root/data/.spark-apps/vllm' && chmod 700 '/root/data/.spark-apps/vllm' && tar -x -C '/root/data/.spark-apps/vllm' -f - && chmod 600 '/root/data/.spark-apps/vllm/env'"
	if got != want {
		t.Fatalf("UploadCommand =\n%s\nwant\n%s", got, want)
	}
}
