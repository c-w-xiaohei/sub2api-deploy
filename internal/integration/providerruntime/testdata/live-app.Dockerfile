# CI-only live-runtime probe image. It is not a production Sub2API image.
FROM postgres:18-alpine
RUN apk add --no-cache busybox redis
COPY live-app.sh /usr/local/bin/live-app
RUN chmod 0755 /usr/local/bin/live-app
ENTRYPOINT ["/usr/local/bin/live-app"]
