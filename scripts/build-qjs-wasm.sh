#!/bin/sh
# Rebuild internal/qjs/qjs.wasm from the pinned quickjs-ng commit.
#
#   scripts/build-qjs-wasm.sh [work-dir]
#
# The glue in internal/qjs/qjswasm/ is compiled against quickjs-ng
# d01ca4491fb24ccfeccb4c7394e28a3b21fd5986 with the Homebrew WASI toolchain
# (llvm, wasi-libc, wasi-runtimes) and optimized with wasm-opt -O3. The result
# is not byte-identical to upstream's wasi-sdk 20 build, so behavior, not
# bytes, is pinned by `go test ./internal/qjs`. Set QJS_QUICKJS_REPO to clone
# from a mirror; a pre-populated <work-dir>/qjswasm/quickjs at the pinned
# commit builds offline.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
work=${1:-${TMPDIR:-/tmp}/flats-qjs-wasm}
commit=d01ca4491fb24ccfeccb4c7394e28a3b21fd5986
repo=${QJS_QUICKJS_REPO:-https://github.com/quickjs-ng/quickjs.git}
glue=$work/qjswasm
quickjs=$glue/quickjs
build=$quickjs/build
out=$root/internal/qjs/qjs.wasm

die() {
	echo "build-qjs-wasm: $*" >&2
	exit 1
}

brew_prefix() {
	if p=$(brew --prefix "$1" 2>/dev/null); then
		printf '%s\n' "$p"
		return 0
	fi
	for p in "/home/linuxbrew/.linuxbrew/opt/$1" "/opt/homebrew/opt/$1" "/usr/local/opt/$1"; do
		if [ -d "$p" ]; then
			printf '%s\n' "$p"
			return 0
		fi
	done
	return 1
}

llvm=$(brew_prefix llvm) || die "Homebrew llvm is missing: brew install llvm"
wasilibc=$(brew_prefix wasi-libc) || die "wasi-libc is missing: brew install wasi-libc"
wasiruntimes=$(brew_prefix wasi-runtimes) || die "wasi-runtimes is missing: brew install wasi-runtimes"
for tool in cmake ninja wasm-opt git; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is missing: brew install cmake ninja binaryen git"
done

clang=$llvm/bin/clang
sysroot=$wasilibc/share/wasi-sysroot
runtimes=$wasiruntimes/share/wasi-runtimes
for path in "$clang" "$llvm/bin/llvm-ar" "$llvm/bin/llvm-ranlib" "$sysroot" \
	"$runtimes/lib/wasm32-unknown-wasip1"; do
	[ -e "$path" ] || die "missing toolchain path $path"
done

if [ ! -d "$quickjs/.git" ]; then
	mkdir -p "$quickjs"
	git -C "$quickjs" init -q
	git -C "$quickjs" remote add origin "$repo"
	git -C "$quickjs" fetch -q --depth 1 origin "$commit"
	git -C "$quickjs" checkout -q FETCH_HEAD
fi
have=$(git -C "$quickjs" rev-parse HEAD)
[ "$have" = "$commit" ] || die "$quickjs is at $have, want pinned $commit"

mkdir -p "$work/tc"
cp "$root"/internal/qjs/qjswasm/*.c "$root"/internal/qjs/qjswasm/*.h \
	"$root"/internal/qjs/qjswasm/qjswasm.cmake "$glue/"
cat >"$work/tc/wasi.cmake" <<EOF
set(CMAKE_SYSTEM_NAME WASI)
set(CMAKE_SYSTEM_VERSION 1)
set(CMAKE_SYSTEM_PROCESSOR wasm32)
set(CMAKE_C_COMPILER $clang)
set(CMAKE_AR $llvm/bin/llvm-ar)
set(CMAKE_RANLIB $llvm/bin/llvm-ranlib)
set(CMAKE_C_COMPILER_TARGET wasm32-wasip1)
set(CMAKE_SYSROOT $sysroot)
set(CMAKE_EXE_LINKER_FLAGS_INIT "-fuse-ld=lld -L$runtimes/lib/wasm32-unknown-wasip1 -resource-dir=$runtimes")
set(CMAKE_TRY_COMPILE_TARGET_TYPE STATIC_LIBRARY)
set(CMAKE_FIND_ROOT_PATH_MODE_PROGRAM NEVER)
EOF

cmake -S "$quickjs" -B "$build" -G Ninja \
	-DCMAKE_TOOLCHAIN_FILE="$work/tc/wasi.cmake" \
	-DCMAKE_PROJECT_INCLUDE="$glue/qjswasm.cmake" \
	-DQJS_BUILD_LIBC=ON \
	-DCMAKE_POLICY_VERSION_MINIMUM=3.5
ninja -C "$build" qjswasm
wasm-opt -O3 "$build/qjswasm" -o "$out.tmp"
mv "$out.tmp" "$out"
echo "wrote $out"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum "$out"
else
	shasum -a 256 "$out"
fi
echo "verify with: go test ./internal/qjs"
