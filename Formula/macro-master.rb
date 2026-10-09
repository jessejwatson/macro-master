class MacroMaster < Formula
  desc "Save and run macro commands, with git-synced shared libraries"
  homepage "https://github.com/jessejwatson/macro-master"
  url "https://github.com/jessejwatson/macro-master/archive/refs/tags/v0.1.4.tar.gz"
  sha256 "faf368ca94bb575cf87d6b76ef8294424a177d325d177839483b4d3da56b9b72"
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
      `mm -d <name>` runs a macro detached; `mm attach` reconnects to it.
      `mm config` opens the settings panel.

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

    assert_equal "#{testpath}/mm/config.json\n", shell_output("#{bin}/mm config path")
    (testpath/"mm/config.json").write '{"default_library":"personal","jobs":{"notify":"off"}}'
    assert_match "jobs.notify = off", shell_output("#{bin}/mm config")

    # A detached job runs on its own terminal and keeps its output.
    assert_match "Started job 1", shell_output("#{bin}/mm -d hi who=job 2>&1")
    20.times do
      break if shell_output("#{bin}/mm jobs").include?("exit 0")

      sleep 0.5
    end
    assert_match "hello job", shell_output("#{bin}/mm attach 1")
  end
end
