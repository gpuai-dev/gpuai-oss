package imageref

import (
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		in                  string
		registry, repo, tag string
		digest              string
		canonical           string
	}{
		{"ubuntu", "docker.io", "library/ubuntu", "latest", "", "ubuntu:latest"},
		{"ubuntu:22.04", "docker.io", "library/ubuntu", "22.04", "", "ubuntu:22.04"},
		{"pytorch/pytorch:2.4.1-cuda12.4-cudnn9-devel", "docker.io", "pytorch/pytorch", "2.4.1-cuda12.4-cudnn9-devel", "", "pytorch/pytorch:2.4.1-cuda12.4-cudnn9-devel"},
		{"ghcr.io/acme/trainer:v1", "ghcr.io", "acme/trainer", "v1", "", "ghcr.io/acme/trainer:v1"},
		{"GHCR.io/acme/trainer:v1", "ghcr.io", "acme/trainer", "v1", "", "ghcr.io/acme/trainer:v1"}, // hosts are case-insensitive
		{"nvcr.io/nvidia/pytorch:24.05-py3", "nvcr.io", "nvidia/pytorch", "24.05-py3", "", "nvcr.io/nvidia/pytorch:24.05-py3"},
		{"registry.example.com:5000/team/app", "registry.example.com:5000", "team/app", "latest", "", "registry.example.com:5000/team/app:latest"},
		{
			"ghcr.io/acme/trainer@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"ghcr.io", "acme/trainer", "",
			"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"ghcr.io/acme/trainer@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	for _, c := range cases {
		ref, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): unexpected error %v", c.in, err)
			continue
		}
		if ref.Registry != c.registry || ref.Repository != c.repo || ref.Tag != c.tag || ref.Digest != c.digest {
			t.Errorf("Parse(%q) = %+v, want registry=%q repo=%q tag=%q digest=%q", c.in, ref, c.registry, c.repo, c.tag, c.digest)
		}
		if got := ref.String(); got != c.canonical {
			t.Errorf("Parse(%q).String() = %q, want %q", c.in, got, c.canonical)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"",
		"UBUNTU",                                // uppercase repo
		"ubuntu:tag with space",                 // space
		"ubuntu:22.04; rm -rf /",                // shell metachars
		"ubuntu:$(whoami)",                      // shell expansion
		"ubuntu@sha256:short",                   // bad digest
		"ubuntu@md5:" + strings.Repeat("a", 64), // wrong algo
		"localhost/evil:1",                      // localhost registry
		"localhost:5000/evil:1",                 // localhost with port
		"127.0.0.1/evil:1",                      // loopback IP
		"10.0.0.8:5000/evil:1",                  // private IP
		"169.254.169.254/latest:1",              // link-local (metadata service)
		"100.64.0.5/evil:1",                     // CGNAT (secondary VPC CIDR)
		"192.0.2.10/evil:1",                     // TEST-NET-1
		"240.0.0.1/evil:1",                      // class E
		"reg.example.com:99999/evil:1",          // port out of range
		"registry/evil:1",                       // single-label host is a Hub repo... (see below)
		"internal:5000/evil:1",                  // single-label host with port
		"-bad/leading:1",                        // bad path component
		"a//b:1",                                // empty component
		strings.Repeat("a", MaxRefLen+1),        // too long
		"ubuntu:" + strings.Repeat("t", 129),    // tag too long
	}
	for _, in := range bad {
		if _, err := Parse(in); err == nil {
			// "registry/evil:1" parses as a Hub repo (registry ∕ evil), which
			// IS valid — docker's own rule. Skip that one from the reject set.
			if in == "registry/evil:1" {
				continue
			}
			t.Errorf("Parse(%q): expected error, got nil", in)
		}
	}
}

func TestParseHubShorthandStaysHub(t *testing.T) {
	// "registry/evil" has no dot/colon in the first component, so it is a
	// Docker Hub repository, never a registry host.
	ref, err := Parse("registry/evil:1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Registry != "docker.io" || ref.Repository != "registry/evil" {
		t.Fatalf("got %+v, want docker.io registry/evil", ref)
	}
}

func TestPinned(t *testing.T) {
	ref, err := Parse("ghcr.io/acme/trainer:v1")
	if err != nil {
		t.Fatal(err)
	}
	d := "sha256:" + strings.Repeat("b", 64)
	got := ref.Pinned(d)
	want := "ghcr.io/acme/trainer:v1@" + d
	if got != want {
		t.Fatalf("Pinned = %q, want %q", got, want)
	}
	// Hub shorthand keeps its familiar form even when pinned.
	hub, _ := Parse("ubuntu")
	if got := hub.Pinned(d); got != "ubuntu@"+d {
		t.Fatalf("hub Pinned = %q", got)
	}
}
