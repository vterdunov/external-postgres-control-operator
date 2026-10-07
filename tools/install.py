"""Install project tools without taking ownership of global mise configuration."""

import json
import subprocess


def main():
    tools = json.loads(subprocess.check_output(["mise", "ls", "--local", "--json"]))
    if not tools:
        raise SystemExit("No project tool pins found")
    subprocess.check_call(["mise", "install", "--locked", *sorted(tools)])


if __name__ == "__main__":
    main()
