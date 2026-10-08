class MacroMaster < Formula
  desc "Save and run macro commands, with git-synced shared libraries"
  homepage "https://github.com/jessejwatson/macro-master"
  url "https://github.com/jessejwatson/macro-master/archive/refs/tags/v0.1.3.tar.gz"
  sha256 "2d2168b4f99e3d57e41db01c681133e30076d50a551727c0345836127fa69f91"
  license "MIT"
  head "https://github.com/jessejwatson/macro-master.git", branch: "main"

  depends_on "go" => :build
  depends_on "fzf"
  depends_on "git"

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}", output: bin/"mm"), "./cmd/mm"
    generate_completions_from_executable(bin/"mm", "completion")
  end

  def caveats
    <<~EOS
      Run `mm` to open the picker, or `mm add <name>` to save the clipboard.

      For macros marked `# mode: source` (cd, export and the like), add the
      shell hook to your shell config:
        zsh:  eval "$(mm init zsh)"       in ~/.zshrc
        bash: eval "$(mm init bash)"      in ~/.bashrc
        fish: mm init fish | source       in ~/.config/fish/config.fish
    EOS
  end

  test do
    assert_match "mm #{version}", shell_output("#{bin}/mm --version")
    ENV["MM_HOME"] = testpath/"mm"
    assert_match "personal", shell_output("#{bin}/mm lib ls")
    (testpath/"mm/libraries/personal/hi.sh").write "echo hello {{who:world}}\n"
    assert_equal "hello brew\n", shell_output("#{bin}/mm hi who=brew")
    assert_match "__complete", shell_output("#{bin}/mm completion zsh")
  end
end
