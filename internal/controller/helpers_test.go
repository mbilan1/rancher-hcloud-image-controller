package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("sanitizeName", func() {
	It("lowercases uppercase characters", func() {
		Expect(sanitizeName("MyConfig")).To(Equal("myconfig"))
	})

	It("replaces underscores with hyphens", func() {
		Expect(sanitizeName("my_config_name")).To(Equal("my-config-name"))
	})

	It("strips non-alphanumeric non-hyphen characters", func() {
		Expect(sanitizeName("my.config!@#name")).To(Equal("myconfigname"))
	})

	It("collapses consecutive hyphens", func() {
		Expect(sanitizeName("my---config")).To(Equal("my-config"))
	})

	It("trims leading and trailing hyphens", func() {
		Expect(sanitizeName("-my-config-")).To(Equal("my-config"))
	})

	It("handles empty string", func() {
		Expect(sanitizeName("")).To(Equal(""))
	})

	It("handles string with only special characters", func() {
		Expect(sanitizeName("!!!")).To(Equal(""))
	})

	It("handles already valid DNS-1123 name", func() {
		Expect(sanitizeName("my-valid-name")).To(Equal("my-valid-name"))
	})

	It("handles mixed special characters and underscores", func() {
		Expect(sanitizeName("NC_pool-1__v2!")).To(Equal("nc-pool-1-v2"))
	})
})

var _ = Describe("deterministicJobName", func() {
	It("produces deterministic output for same inputs", func() {
		name1 := deterministicJobName("my-config", "cis", "v1.32.4+rke2r1")
		name2 := deterministicJobName("my-config", "cis", "v1.32.4+rke2r1")
		Expect(name1).To(Equal(name2))
	})

	It("produces different names for different configs", func() {
		name1 := deterministicJobName("config-a", "cis", "v1.32.4+rke2r1")
		name2 := deterministicJobName("config-b", "cis", "v1.32.4+rke2r1")
		Expect(name1).NotTo(Equal(name2))
	})

	It("produces different names for different specs", func() {
		name1 := deterministicJobName("my-config", "cis", "v1.32.4+rke2r1")
		name2 := deterministicJobName("my-config", "base", "v1.32.4+rke2r1")
		Expect(name1).NotTo(Equal(name2))
	})

	It("starts with img-build- prefix", func() {
		name := deterministicJobName("my-config", "cis", "v1.32.4+rke2r1")
		Expect(name).To(HavePrefix("img-build-"))
	})

	It("is at most 63 characters", func() {
		name := deterministicJobName("a-very-long-config-name-that-exceeds-typical-lengths-for-k8s-resources", "cis", "v1.32.4+rke2r1")
		Expect(len(name)).To(BeNumerically("<=", 63))
	})

	It("does not end with a hyphen after truncation", func() {
		name := deterministicJobName("a-very-long-config-name-that-exceeds-typical-lengths-for-k8s-resources", "cis", "v1.32.4+rke2r1")
		Expect(name).NotTo(HaveSuffix("-"))
	})

	It("changes when RKE2 version changes", func() {
		name1 := deterministicJobName("my-config", "cis", "v1.32.4+rke2r1")
		name2 := deterministicJobName("my-config", "cis", "v1.33.0+rke2r1")
		Expect(name1).NotTo(Equal(name2))
	})
})

var _ = Describe("buildJobEnv", func() {
	It("always includes HCLOUD_TOKEN and ENABLE_CIS", func() {
		r := &HetznerConfigReconciler{Config: Config{}}
		envs := r.buildJobEnv("my-secret", "true", "")
		names := envVarNames(envs)
		Expect(names).To(ContainElements("HCLOUD_TOKEN", "ENABLE_CIS"))
	})

	It("references the correct secret for HCLOUD_TOKEN", func() {
		r := &HetznerConfigReconciler{Config: Config{}}
		envs := r.buildJobEnv("cred-secret", "false", "")
		var tokenEnv *envVar
		for i := range envs {
			if envs[i].Name == "HCLOUD_TOKEN" {
				tokenEnv = &envVar{envs[i].Name, envs[i].ValueFrom}
				break
			}
		}
		Expect(tokenEnv).NotTo(BeNil())
		Expect(tokenEnv.ValueFrom).NotTo(BeNil())
		Expect(tokenEnv.ValueFrom.SecretKeyRef.Name).To(Equal("cred-secret"))
		Expect(tokenEnv.ValueFrom.SecretKeyRef.Key).To(Equal("HCLOUD_TOKEN"))
	})

	It("includes RKE2_VERSION when provided", func() {
		r := &HetznerConfigReconciler{Config: Config{}}
		envs := r.buildJobEnv("s", "false", "v1.32.4+rke2r1")
		Expect(envVarNames(envs)).To(ContainElement("RKE2_VERSION"))
		Expect(envVarValue(envs, "RKE2_VERSION")).To(Equal("v1.32.4+rke2r1"))
	})

	It("omits RKE2_VERSION when empty", func() {
		r := &HetznerConfigReconciler{Config: Config{}}
		envs := r.buildJobEnv("s", "false", "")
		Expect(envVarNames(envs)).NotTo(ContainElement("RKE2_VERSION"))
	})

	It("includes LOCATION when configured", func() {
		r := &HetznerConfigReconciler{Config: Config{Location: "fsn1"}}
		envs := r.buildJobEnv("s", "false", "")
		Expect(envVarValue(envs, "LOCATION")).To(Equal("fsn1"))
	})

	It("omits LOCATION when empty", func() {
		r := &HetznerConfigReconciler{Config: Config{}}
		envs := r.buildJobEnv("s", "false", "")
		Expect(envVarNames(envs)).NotTo(ContainElement("LOCATION"))
	})

	It("includes SERVER_TYPE when configured", func() {
		r := &HetznerConfigReconciler{Config: Config{ServerType: "cx23"}}
		envs := r.buildJobEnv("s", "false", "")
		Expect(envVarValue(envs, "SERVER_TYPE")).To(Equal("cx23"))
	})

	It("includes BASE_IMAGE when configured", func() {
		r := &HetznerConfigReconciler{Config: Config{BaseImage: "ubuntu-24.04"}}
		envs := r.buildJobEnv("s", "false", "")
		Expect(envVarValue(envs, "BASE_IMAGE")).To(Equal("ubuntu-24.04"))
	})

	It("includes all optional vars when fully configured", func() {
		r := &HetznerConfigReconciler{Config: Config{
			Location:   "fsn1",
			ServerType: "cx23",
			BaseImage:  "ubuntu-24.04",
		}}
		envs := r.buildJobEnv("s", "true", "v1.32.4+rke2r1")
		names := envVarNames(envs)
		Expect(names).To(ContainElements("HCLOUD_TOKEN", "ENABLE_CIS", "RKE2_VERSION", "LOCATION", "SERVER_TYPE", "BASE_IMAGE"))
	})
})
