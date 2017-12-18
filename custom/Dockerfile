FROM gliderlabs/logspout:master

COPY entrypoint.sh /src/entrypoint.sh
RUN chmod +x /src/entrypoint.sh

ENTRYPOINT ["/src/entrypoint.sh"]
