FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ink . && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ink /ink
# Docker seeds a new named volume with this directory's ownership, so the nonroot user can write the database.
COPY --from=build --chown=65532:65532 /data /data
ENV ADDR=:8080 DB_PATH=/data/ink.db TRUSTED_PROXY_HEADER=X-Forwarded-For
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/ink"]
