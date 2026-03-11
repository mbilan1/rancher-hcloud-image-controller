package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("findSnapshot", func() {
	var (
		server *httptest.Server
		r      *HetznerConfigReconciler
	)

	AfterEach(func() {
		if server != nil {
			server.Close()
		}
	})

	setup := func(handler http.HandlerFunc) {
		server = httptest.NewServer(handler)
		r = &HetznerConfigReconciler{
			HTTPClient: server.Client(),
			Config:     Config{RKE2Version: "v1.32.4+rke2r1"},
		}
	}

	It("returns snapshot ID when a matching available snapshot exists", func() {
		setup(func(w http.ResponseWriter, req *http.Request) {
			Expect(req.URL.Path).To(Equal("/v1/images"))
			Expect(req.URL.Query().Get("type")).To(Equal("snapshot"))
			Expect(req.URL.Query().Get("sort")).To(Equal("created:desc"))
			Expect(req.URL.Query().Get("label_selector")).To(ContainSubstring("rke2-version=v1.32.4-rke2r1"))
			Expect(req.URL.Query().Get("label_selector")).To(ContainSubstring("cis-hardened=true"))
			Expect(req.Header.Get("Authorization")).To(Equal("Bearer test-token"))

			resp := hetznerImageResponse{
				Images: []hetznerImage{
					{ID: 12345, Status: "available", Created: "2025-01-01T00:00:00+00:00"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		})

		// Override the base URL by pointing HTTPClient at test server
		// findSnapshot uses hardcoded "https://api.hetzner.cloud/v1/images"
		// so we need a custom transport to redirect to test server
		r.HTTPClient = &http.Client{
			Transport: &rewriteTransport{
				base:    http.DefaultTransport,
				baseURL: server.URL,
			},
		}

		id, err := r.findSnapshot(ctx(), "test-token", "v1.32.4+rke2r1", true)
		Expect(err).NotTo(HaveOccurred())
		Expect(id).To(Equal("12345"))
	})

	It("returns empty string when no snapshots match", func() {
		setup(func(w http.ResponseWriter, _ *http.Request) {
			resp := hetznerImageResponse{Images: []hetznerImage{}}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		})
		r.HTTPClient = &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		id, err := r.findSnapshot(ctx(), "test-token", "v1.32.4+rke2r1", false)
		Expect(err).NotTo(HaveOccurred())
		Expect(id).To(BeEmpty())
	})

	It("skips snapshots that are not available", func() {
		setup(func(w http.ResponseWriter, _ *http.Request) {
			resp := hetznerImageResponse{
				Images: []hetznerImage{
					{ID: 111, Status: "creating", Created: "2025-01-02T00:00:00+00:00"},
					{ID: 222, Status: "available", Created: "2025-01-01T00:00:00+00:00"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		})
		r.HTTPClient = &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		id, err := r.findSnapshot(ctx(), "test-token", "v1.32.4+rke2r1", true)
		Expect(err).NotTo(HaveOccurred())
		Expect(id).To(Equal("222"))
	})

	It("returns error on non-200 response", func() {
		setup(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"message":"forbidden"}}`))
		})
		r.HTTPClient = &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		_, err := r.findSnapshot(ctx(), "bad-token", "v1.32.4+rke2r1", false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("403"))
	})

	It("returns error on invalid JSON response", func() {
		setup(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`not json`))
		})
		r.HTTPClient = &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		_, err := r.findSnapshot(ctx(), "test-token", "v1.32.4+rke2r1", false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("decode"))
	})

	It("uses cis-hardened=false label for non-CIS spec", func() {
		setup(func(w http.ResponseWriter, req *http.Request) {
			Expect(req.URL.Query().Get("label_selector")).To(ContainSubstring("cis-hardened=false"))
			resp := hetznerImageResponse{Images: []hetznerImage{}}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		})
		r.HTTPClient = &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		_, err := r.findSnapshot(ctx(), "test-token", "v1.32.4+rke2r1", false)
		Expect(err).NotTo(HaveOccurred())
	})

	It("replaces + with - in rke2 version label", func() {
		setup(func(w http.ResponseWriter, req *http.Request) {
			Expect(req.URL.Query().Get("label_selector")).To(ContainSubstring("rke2-version=v1.32.4-rke2r1"))
			resp := hetznerImageResponse{Images: []hetznerImage{}}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		})
		r.HTTPClient = &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		_, err := r.findSnapshot(ctx(), "test-token", "v1.32.4+rke2r1", true)
		Expect(err).NotTo(HaveOccurred())
	})
})
