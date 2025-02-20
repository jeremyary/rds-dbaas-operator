# Build the manager binary
FROM registry.access.redhat.com/ubi8/go-toolset:1.18.4 AS builder

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/

# Copy the RDS CRDs
COPY rds/config/common/bases/ crds/
# Copy the DBaaS CRs
COPY rds/dbaas/dbaasprovider crs/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

# Build the operator image
FROM registry.access.redhat.com/ubi8-minimal:8.7

COPY LICENSE /licenses/LICENSE
WORKDIR /
COPY --from=builder /opt/app-root/src/manager .
COPY --from=builder /opt/app-root/src/crds/* ./
COPY --from=builder /opt/app-root/src/crs/* ./
USER 65532:65532

ENTRYPOINT ["/manager"]
