"""Link repository-private skills into the current Git checkout without replacing files."""

from pathlib import Path
import subprocess


def git_path(argument):
    return Path(subprocess.check_output(
        ["git", "rev-parse", "--path-format=absolute", argument], text=True,
    ).strip())


def main():
    root = git_path("--show-toplevel")
    catalog = git_path("--git-common-dir") / "agent-skills"
    if not catalog.exists():
        return
    if not catalog.is_dir():
        raise SystemExit(f"Private skill catalog is not a directory: {catalog}")
    sources = [source for source in sorted(catalog.iterdir())
               if (source / "SKILL.md").is_file()]
    if not sources:
        return
    destination = root / ".agents" / "skills"
    for parent in (destination.parent, destination):
        if parent.is_symlink() or (parent.exists() and not parent.is_dir()):
            raise SystemExit(f"Refusing to provision through an existing non-directory or symlink: {parent}")
    links = []
    for source in sources:
        link = destination / source.name
        canonical_source = source.resolve()
        if link.is_symlink() and link.resolve() == canonical_source:
            continue
        if link.exists() or link.is_symlink():
            raise SystemExit(f"Refusing to replace existing project skill: {link}")
        links.append((link, canonical_source))
    if not links:
        return
    destination.mkdir(parents=True, exist_ok=True)
    for link, source in links:
        link.symlink_to(source, target_is_directory=True)
        print(f"Linked project skill: {link.name}")


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        raise SystemExit(f"Could not provision project skills: {error}") from error
