#!/usr/bin/env bash
# Rebuild internal/qjs/qjs.wasm from the pinned quickjs-ng commit and toolchain.
#
#   scripts/build-qjs-wasm.sh [work-dir]
#
# The glue in internal/qjs/qjswasm/ is compiled against quickjs-ng at the pinned
# commit with the Homebrew WASI toolchain (llvm@23, wasi-libc, wasi-runtimes)
# and optimized with wasm-opt -O3. With the declared toolchain versions, the
# build reproduces the committed qjs.wasm SHA-256 byte-for-byte. Set
# QJS_QUICKJS_REPO to clone from a mirror; a pre-populated
# <work-dir>/qjswasm/quickjs at the pinned commit builds offline.
set -euo pipefail
trap 'rm -f "${out:-}.tmp"' EXIT INT TERM

root=$(cd "$(dirname "$0")/.." && pwd)
work=${1:-${TMPDIR:-/tmp}/flats-qjs-wasm}
commit=d01ca4491fb24ccfeccb4c7394e28a3b21fd5986
repo=${QJS_QUICKJS_REPO:-https://github.com/quickjs-ng/quickjs.git}
glue=$work/qjswasm
quickjs=$glue/quickjs
build=$quickjs/build
out=${QJS_WASM_OUT:-$root/internal/qjs/qjs.wasm}

pinned_wasm_sha256="79284a9282ad8471573a4d52401362e27d149f1d70b4175cb5affc960a8dc9a9"
llvm_formula="llvm@23"
lld_formula="lld@23"
pinned_clang_major="23"
pinned_lld_major="23"
pinned_clang_version="Homebrew clang version 23.1.4"
pinned_wasm_ld_version="Homebrew LLD 23.1.4"
pinned_cmake_version="cmake version 4.4.5"
pinned_ninja_version="1.13.2"
pinned_wasm_opt_version="wasm-opt version 120"

die() {
	echo "build-qjs-wasm: $*" >&2
	exit 1
}

calc_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "neither sha256sum nor shasum is available"
	fi
}

brew_prefix() {
	local p
	if p=$(brew --prefix "$1" 2>/dev/null) && [ -d "$p" ]; then
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

if ! llvm=$(brew_prefix "$llvm_formula"); then
	if p=$(brew_prefix llvm); then
		v=$("$p/bin/clang" --version 2>/dev/null | head -n 1 || true)
		case "$v" in
		*" $pinned_clang_major."*) llvm="$p" ;;
		*) die "installed Homebrew llvm ($p, $v) differs from $llvm_formula: brew install $llvm_formula" ;;
		esac
	else
		die "Homebrew $llvm_formula is missing: brew install $llvm_formula"
	fi
fi

if ! lld=$(brew_prefix "$lld_formula"); then
	if p=$(brew_prefix lld); then
		v=$("$p/bin/wasm-ld" --version 2>/dev/null | head -n 1 || true)
		case "$v" in
		*" $pinned_lld_major."*) lld="$p" ;;
		*) die "installed Homebrew lld ($p, $v) differs from $lld_formula: brew install $lld_formula" ;;
		esac
	else
		die "Homebrew $lld_formula is missing: brew install $lld_formula"
	fi
fi

wasilibc=$(brew_prefix wasi-libc) || die "wasi-libc is missing: brew install wasi-libc"
wasiruntimes=$(brew_prefix wasi-runtimes) || die "wasi-runtimes is missing: brew install wasi-runtimes"
for tool in cmake ninja wasm-opt git; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is missing: brew install cmake ninja binaryen git"
done

export PATH="$lld/bin:$llvm/bin:$PATH"
clang=$llvm/bin/clang
wasm_ld=$lld/bin/wasm-ld
if [ ! -x "$wasm_ld" ]; then
	wasm_ld=$(command -v wasm-ld 2>/dev/null || true)
fi
[ -n "$wasm_ld" ] && [ -x "$wasm_ld" ] || die "wasm-ld is missing: brew install $lld_formula"

sysroot=$wasilibc/share/wasi-sysroot
runtimes=$wasiruntimes/share/wasi-runtimes
for path in "$clang" "$wasm_ld" "$llvm/bin/llvm-ar" "$llvm/bin/llvm-ranlib" "$sysroot" \
	"$runtimes/lib/wasm32-unknown-wasip1"; do
	[ -e "$path" ] || die "missing toolchain path $path"
done

have_clang_version=$("$clang" --version | head -n 1)
have_wasm_ld_version=$("$wasm_ld" --version | head -n 1)
have_cmake_version=$(cmake --version | head -n 1)
have_ninja_version=$(ninja --version | head -n 1)
have_wasm_opt_version=$(wasm-opt --version | head -n 1)

echo "toolchain versions:"
echo "  clang:    $have_clang_version"
echo "  wasm-ld:  $have_wasm_ld_version"
echo "  cmake:    $have_cmake_version"
echo "  ninja:    $have_ninja_version"
echo "  wasm-opt: $have_wasm_opt_version"

toolchain_matches=1
if [ "$have_clang_version" != "$pinned_clang_version" ] || \
   [ "$have_wasm_ld_version" != "$pinned_wasm_ld_version" ] || \
   [ "$have_cmake_version" != "$pinned_cmake_version" ] || \
   [ "$have_ninja_version" != "$pinned_ninja_version" ] || \
   [ "$have_wasm_opt_version" != "$pinned_wasm_opt_version" ]; then
	toolchain_matches=0
fi

if [ "$toolchain_matches" -eq 0 ]; then
	echo "toolchain differences from pins:" >&2
	[ "$have_clang_version" = "$pinned_clang_version" ] || echo "  clang: want '$pinned_clang_version', have '$have_clang_version'" >&2
	[ "$have_wasm_ld_version" = "$pinned_wasm_ld_version" ] || echo "  wasm-ld: want '$pinned_wasm_ld_version', have '$have_wasm_ld_version'" >&2
	[ "$have_cmake_version" = "$pinned_cmake_version" ] || echo "  cmake: want '$pinned_cmake_version', have '$have_cmake_version'" >&2
	[ "$have_ninja_version" = "$pinned_ninja_version" ] || echo "  ninja: want '$pinned_ninja_version', have '$have_ninja_version'" >&2
	[ "$have_wasm_opt_version" = "$pinned_wasm_opt_version" ] || echo "  wasm-opt: want '$pinned_wasm_opt_version', have '$have_wasm_opt_version'" >&2
fi

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
have_sha256=$(calc_sha256 "$out")
echo "sha256: $have_sha256  $out"

if [ "$have_sha256" = "$pinned_wasm_sha256" ]; then
	echo "wasm sha256 matches pinned value: $pinned_wasm_sha256"
elif [ -n "${QJS_BUMP_TOOLCHAIN:-}" ]; then
	echo "toolchain bump mode: produced wasm sha256 $have_sha256 (pinned: $pinned_wasm_sha256)"
	echo "Update pinned toolchain versions and pinned_wasm_sha256 in scripts/build-qjs-wasm.sh if intentional."
elif [ "$toolchain_matches" -eq 1 ]; then
	die "rebuild drifted: expected sha256 $pinned_wasm_sha256, got $have_sha256 with identical pinned toolchain"
else
	echo "notice: wasm sha256 differs ($pinned_wasm_sha256 -> $have_sha256) under modified toolchain." >&2
	echo "A documented toolchain bump legitimately changes the hash. Update pinned_wasm_sha256 in scripts/build-qjs-wasm.sh if intentional." >&2
fi
echo "verify with: go test ./internal/qjs"
