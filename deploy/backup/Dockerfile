# postgres client tools matching the server (16) + rclone for S3/R2
FROM postgres:16-alpine
RUN apk add --no-cache rclone \
 && mkdir /backups && chown postgres /backups
COPY --chmod=755 backup.sh /usr/local/bin/backup.sh
USER postgres
ENTRYPOINT ["/usr/local/bin/backup.sh"]
