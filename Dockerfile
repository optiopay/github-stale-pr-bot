FROM ubuntu

RUN apt-get update && apt-get install -y ca-certificates
RUN apt-get clean
RUN rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*
COPY ./github-stale-pr-bot /bin/
RUN chmod +x /bin/github-stale-pr-bot
ENTRYPOINT ["/bin/github-stale-pr-bot"]
