# Nightly encrypted backup of the licensing database (Railway cron service
# "pg-backup"; see backup.sh). pg_dump must be at least the server's major
# version: Railway's Postgres is 18.
FROM postgres:18-alpine
RUN apk add --no-cache bash age aws-cli coreutils
COPY backup.sh restore.sh /usr/local/bin/
RUN sed -i 's/\r$//' /usr/local/bin/*.sh && chmod +x /usr/local/bin/backup.sh /usr/local/bin/restore.sh
USER postgres
ENTRYPOINT []
CMD ["/usr/local/bin/backup.sh"]
