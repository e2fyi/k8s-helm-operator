# github.com/e2fyi/k8s-helm-operator
[![golang](https://img.shields.io/badge/golang-v1.12-5272B4.svg?style=flat-square "golang v1.12")](https://godoc.org/github.com/e2fyi/minio-web/pkg) 


`k8s-helm-operator` aims to make deploying helm charts more declarative through
k8s Custom Resource Definitions.

## Developer notes

```bash
# create a vendor sub-folder so `go generate` can work
go mod vendor

# generates the deep-copy functions as well as the client codes
go generate
```