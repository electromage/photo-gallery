FROM golang:1.23-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/photo-gallery .

FROM alpine:3.22
WORKDIR /app

# exiftool provides the richest metadata (incl. Nikon/Canon lens names); libwebp-tools
# provides cwebp for smaller WebP thumbnails/previews. Both are optional — without
# them the app falls back to a built-in EXIF reader and JPEG renditions.
RUN apk add --no-cache exiftool libwebp-tools

COPY --from=build /out/photo-gallery /app/photo-gallery

# Run as an unprivileged user. Pre-create the cache/photo mount points owned by
# that user so a fresh named volume (or the ephemeral layer) is writable.
RUN addgroup -S gallery \
	&& adduser -S -G gallery -h /app gallery \
	&& mkdir -p /cache/thumbs /photos \
	&& chown -R gallery:gallery /app /cache

USER gallery:gallery

EXPOSE 8080

ENV ADDR=:8080
ENV PHOTO_ROOT=/photos
# Persist the index cache and generated thumbnails here. Mount a writable volume
# at /cache to make them survive container recreation; otherwise they live in the
# container's ephemeral layer and are regenerated for each fresh container.
ENV GALLERY_CACHE=/cache/index.gob
ENV THUMB_CACHE=/cache/thumbs

CMD ["/app/photo-gallery"]
