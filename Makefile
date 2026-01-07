APP=gobelisk
VERSION := $(shell cat VERSION)

.PHONY: build-armhf
build-armhf:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
	  go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" \
	  -o dist/$(APP) ./cmd/gobelisk

.PHONY: deb
deb: build-armhf
	rm -rf pkgroot
	mkdir -p pkgroot/DEBIAN
	mkdir -p pkgroot/usr/local/bin
	mkdir -p pkgroot/usr/share/gobelisk
	mkdir -p pkgroot/lib/systemd/system
	mkdir -p pkgroot/etc/gobelisk

	# control + maintainer scripts
	cp debian/control pkgroot/DEBIAN/control
	sed -i "s/^Version:.*/Version: $(VERSION)/" pkgroot/DEBIAN/control
	cp debian/postinst pkgroot/DEBIAN/postinst
	cp debian/prerm pkgroot/DEBIAN/prerm
	cp debian/conffiles pkgroot/DEBIAN/conffiles

	# payload
	cp dist/$(APP) pkgroot/usr/local/bin/$(APP)
	cp deployments/systemd/gobelisk.service pkgroot/lib/systemd/system/gobelisk.service
	cp configs/config.yaml pkgroot/usr/share/gobelisk/.	
	cp configs/config.yaml pkgroot/etc/gobelisk/.

	dpkg-deb --root-owner-group --build pkgroot dist/gobelisk_$(VERSION)_armhf.deb

.PHONY: clean
clean:
	rm -rf pkgroot
	rm -rf dist