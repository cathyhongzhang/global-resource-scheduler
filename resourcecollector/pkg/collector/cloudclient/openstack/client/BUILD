load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = ["client.go"],
    importpath = "k8s.io/kubernetes/resourcecollector/pkg/collector/cloudclient/openstack/client",
    visibility = ["//visibility:public"],
    deps = [
        "//vendor/github.com/gophercloud/gophercloud:go_default_library",
        "//vendor/github.com/gophercloud/gophercloud/openstack:go_default_library",
        "//vendor/k8s.io/klog:go_default_library",
    ],
)

filegroup(
    name = "package-srcs",
    srcs = glob(["**"]),
    tags = ["automanaged"],
    visibility = ["//visibility:private"],
)

filegroup(
    name = "all-srcs",
    srcs = [":package-srcs"],
    tags = ["automanaged"],
    visibility = ["//visibility:public"],
)
