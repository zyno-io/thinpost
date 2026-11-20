package edgeapp

import "testing"

func submissionEnv() map[string]string {
	return map[string]string{
		"THINPOST_EDGE_WORKER_ID": "worker-1", "THINPOST_EDGE_SMTP_HOSTNAME": "smtp.example.com", "THINPOST_EDGE_BOUNCE_DOMAIN": "bounce.example.com",
		"THINPOST_EDGE_CONTROL_LISTEN": "10.2.3.4:7443", "THINPOST_EDGE_SMTP_LISTEN": "45.0.1.22:25",
		"THINPOST_EDGE_CONTROLLER_IDENTITIES": "spiffe://thinpost.test/controller/a", "THINPOST_EDGE_CONTROL_CA": "ca.pem", "THINPOST_EDGE_CONTROL_CERT": "control.pem", "THINPOST_EDGE_CONTROL_KEY": "control.key",
		"THINPOST_EDGE_SMTP_CERT": "smtp.pem", "THINPOST_EDGE_SMTP_KEY": "smtp.key", "THINPOST_EDGE_JOURNAL_PATH": "/data/journal", "THINPOST_EDGE_JOURNAL_CAPACITY": "100", "THINPOST_EDGE_CLOCK_MARGIN": "5s",
		"THINPOST_EDGE_JOURNAL_RESERVE_BYTES": "67108864",
	}
}

func TestSubmissionDefaultsOffAndParses(t *testing.T) {
	env := submissionEnv()
	get := func(k string) string { return env[k] }
	c, err := Load(get)
	if err != nil || c.Submission || c.SubmissionInternal.Enabled() {
		t.Fatalf("defaults: %v %+v", err, c)
	}
	env["THINPOST_EDGE_SUBMISSION"] = "false"
	if c, err = Load(get); err != nil || c.Submission {
		t.Fatal("false must keep submission off", err)
	}
	env["THINPOST_EDGE_SUBMISSION"] = "true"
	if c, err = Load(get); err != nil || !c.Submission {
		t.Fatal("true must enable submission", err)
	}
	for _, bad := range []string{"1", "yes", "TRUE", "True", " true"} {
		env["THINPOST_EDGE_SUBMISSION"] = bad
		if _, err = Load(get); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

func TestSubmissionInternalListener(t *testing.T) {
	env := submissionEnv()
	get := func(k string) string { return env[k] }
	set := func(listen string) {
		env["THINPOST_EDGE_SUBMISSION_INTERNAL_LISTEN"] = listen
		env["THINPOST_EDGE_SUBMISSION_INTERNAL_HOSTNAME"] = "submission.cluster.example"
		env["THINPOST_EDGE_SUBMISSION_INTERNAL_CERT"] = "/tls/tls.crt"
		env["THINPOST_EDGE_SUBMISSION_INTERNAL_KEY"] = "/tls/tls.key"
		env["THINPOST_EDGE_SUBMISSION_INTERNAL_CA"] = "/tls/ca.crt"
	}
	set("10.2.3.4:587")
	c, err := Load(get)
	if err != nil || !c.SubmissionInternal.Enabled() || c.SubmissionInternal.Listen.String() != "10.2.3.4:587" || c.SubmissionInternal.Hostname != "submission.cluster.example" || c.Submission {
		t.Fatalf("internal-only: %v %+v", err, c.SubmissionInternal)
	}
	for _, bad := range []string{"45.0.1.22:587", "0.0.0.0:587", ":587", "10.2.3.4", "10.2.3.4:0", "10.2.3.4:25", "10.2.3.4:7443", "[fe80::1%eth0]:587", "smtp.example.com:587"} {
		set(bad)
		if _, err = Load(get); err == nil {
			t.Fatalf("listener %q was accepted", bad)
		}
	}
	set("10.2.3.4:587")
	for _, missing := range []string{"HOSTNAME", "CERT", "KEY", "CA"} {
		saved := env["THINPOST_EDGE_SUBMISSION_INTERNAL_"+missing]
		delete(env, "THINPOST_EDGE_SUBMISSION_INTERNAL_"+missing)
		if _, err = Load(get); err == nil {
			t.Fatalf("missing %s was accepted", missing)
		}
		env["THINPOST_EDGE_SUBMISSION_INTERNAL_"+missing] = saved
	}
	env["THINPOST_EDGE_SUBMISSION_INTERNAL_HOSTNAME"] = "Bad_Name"
	if _, err = Load(get); err == nil {
		t.Fatal("invalid hostname was accepted")
	}
	// Settings without the listener are a mistake, not silently ignored.
	set("")
	delete(env, "THINPOST_EDGE_SUBMISSION_INTERNAL_LISTEN")
	if _, err = Load(get); err == nil {
		t.Fatal("internal settings without a listener were accepted")
	}
}
