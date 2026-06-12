# Homebrew formula for sfs.
#
# This is the source-build formula for the runbear-io/homebrew-tap repo.
# Releases via goreleaser (.goreleaser.yaml) generate a bottle-style formula
# with prebuilt binaries automatically; this file is the manual fallback and
# the template for the first tap publication. Update `url` and `sha256` per
# release (sha256: `curl -L <url> | shasum -a 256`).
class Sfs < Formula
  desc "Synced file system for AI agents: mount, sync, and track folders"
  homepage "https://github.com/runbear-io/sfs"
  url "https://github.com/runbear-io/sfs/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000" # update per release
  license "MIT"
  head "https://github.com/runbear-io/sfs.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}"), "./cmd/sfs"
  end

  test do
    assert_match "sfs", shell_output("#{bin}/sfs version")
    ENV["SFS_HOME"] = testpath/".sfs"
    system bin/"sfs", "whoami"
  end
end
