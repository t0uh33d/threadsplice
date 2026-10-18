# Relay image: a static binary on distroless, running as non-root.
FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/threadsplice ./cmd/threadsplice && mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/threadsplice /threadsplice
# Named volumes inherit this ownership on first use, so the nonroot user can write the database.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/threadsplice"]
CMD ["relay", "-addr", ":8080", "-db", "/data/threadsplice.db"]
