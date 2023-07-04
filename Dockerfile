FROM golang:stretch as go

RUN apt-get update && apt-get install -y ca-certificates git && rm -rf /var/cache/apk/*
RUN echo "nobody:x:65534:65534:Nobody:/:" > /etc_passwd
ADD . /go/src/gitlab.com/4mlg/ingress-controller
WORKDIR /go/src/gitlab.com/4mlg/ingress-controller
RUN CGO_ENABLED=0 GOOS=linux GARCH=amd64 GO111MODULE=on go build -mod vendor -a -installsuffix cgo

FROM scratch
WORKDIR /
COPY --from=0 /go/src/gitlab.com/4mlg/ingress-controller/ingress-controller /
COPY --from=0 /etc/ssl/certs /etc/ssl/certs
COPY --from=0 /etc_passwd /etc/passwd
ENTRYPOINT ["/ingress-controller"]