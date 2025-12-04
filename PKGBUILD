# Maintainer: Ken-Oleander Niin <kenoleander@gmail.com>
pkgname=fily
pkgver=1.0.0
pkgrel=1
pkgdesc="A peer-to-peer file transfer CLI tool using WebRTC"
arch=('x86_64' 'aarch64')
url="https://github.com/Kennubyte/fily-cli"
license=('MIT')
depends=('glibc')
makedepends=('go' 'git')
source=("git+https://github.com/Kennubyte/fily-cli.git#tag=v${pkgver}")
sha256sums=('SKIP')

build() {
    cd "$srcdir/fily-cli"
    export CGO_CPPFLAGS="${CPPFLAGS}"
    export CGO_CFLAGS="${CFLAGS}"
    export CGO_CXXFLAGS="${CXXFLAGS}"
    export CGO_LDFLAGS="${LDFLAGS}"
    export GOFLAGS="-buildmode=pie -trimpath -ldflags=-linkmode=external -mod=readonly -modcacherw"
    go build -o "$pkgname" .
}

package() {
    cd "$srcdir/fily-cli"
    install -Dm755 "$pkgname" "$pkgdir/usr/bin/$pkgname"
}
