FROM fedora:26

RUN dnf install -y libgo

RUN dnf install -y geoipupdate
RUN mkdir /geoip/
COPY GeoIP.conf /geoip/
RUN geoipupdate -f /geoip/GeoIP.conf -d /geoip/

COPY geoip /usr/local/bin/

WORKDIR /geoip/

ENTRYPOINT ["/usr/local/bin/geoip"]
CMD [ "/queue/input" ]

