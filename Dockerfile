FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /alma-relay .

FROM scratch
COPY --from=build /alma-relay /alma-relay
USER 65534:65534
ENV HOST=0.0.0.0 PORT=8787
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/alma-relay", "-healthcheck"]
ENTRYPOINT ["/alma-relay"]
