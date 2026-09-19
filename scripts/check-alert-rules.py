"""Check the deployed Prometheus rules against the local ones.

There are two copies of these rules and that is deliberate:
`observability/prometheus/rules/` is what `make up` evaluates, and
`infrastructure/kubernetes/monitoring/config/rules/` is what a deployed
Prometheus evaluates. They must agree about what they measure and are
allowed to disagree about how patient they are, because the local file says
so itself: "The `for` clauses are short here because a local stack is
watched for minutes, not weeks. A deployment lengthens them."

Without a check, two copies drift and nobody notices until an alert that
fires locally is missing in production. So:

  recording rules  must be identical, parsed. They are pure derivations, and
                   an alert and a dashboard that share one cannot be allowed
                   to disagree about what "the error rate" means.

  alerting rules   must have the same alert names, the same expressions and
                   the same severities, and every deployed `for` must be at
                   least the local one. Annotations may differ: they are
                   prose for whoever is woken up.

Run from scripts/k8s-validate.sh. Python because the check needs a YAML
parser and that stage of validation has no Go or Rust toolchain; it runs in
the Checkov image, which the same script already requires.

Exits 0 when the two agree, 1 with a description of each disagreement.
"""

import sys

import yaml

UNITS = {"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800, "y": 31536000}


def seconds(value):
    """Parse a Prometheus duration such as 30s, 5m or 1h30m."""
    if value is None:
        return 0
    text = str(value).strip()
    if not text:
        return 0
    total = 0
    number = ""
    for char in text:
        if char.isdigit():
            number += char
            continue
        if char not in UNITS or not number:
            raise ValueError("cannot read duration %r" % text)
        total += int(number) * UNITS[char]
        number = ""
    if number:
        raise ValueError("duration %r has no unit" % text)
    return total


def load(path):
    with open(path, encoding="utf-8") as handle:
        return yaml.safe_load(handle)


def rules_by_key(document, key):
    """Flatten groups into {name: rule}, keyed by the record or alert name."""
    out = {}
    for group in document.get("groups") or []:
        for rule in group.get("rules") or []:
            if key in rule:
                out[rule[key]] = (group.get("name"), rule)
    return out


def normalise(expr):
    """Compare expressions by their tokens, so indentation and the trailing
    newline of a block scalar do not count as a difference."""
    return " ".join(str(expr).split())


def check_recording(local_path, deployed_path, problems):
    local = rules_by_key(load(local_path), "record")
    deployed = rules_by_key(load(deployed_path), "record")

    for name in sorted(set(local) - set(deployed)):
        problems.append("recording rule %s is local-only; the deployed Prometheus would not have it" % name)
    for name in sorted(set(deployed) - set(local)):
        problems.append("recording rule %s is deployed-only; it is not evaluated locally, so nothing exercises it" % name)

    for name in sorted(set(local) & set(deployed)):
        local_group, local_rule = local[name]
        deployed_group, deployed_rule = deployed[name]
        if local_group != deployed_group:
            problems.append("recording rule %s is in group %r locally and %r deployed" % (name, local_group, deployed_group))
        if normalise(local_rule.get("expr")) != normalise(deployed_rule.get("expr")):
            problems.append(
                "recording rule %s has different expressions:\n      local:    %s\n      deployed: %s"
                % (name, normalise(local_rule.get("expr")), normalise(deployed_rule.get("expr")))
            )
        if local_rule.get("labels") != deployed_rule.get("labels"):
            problems.append("recording rule %s has different labels" % name)


def check_alerts(local_path, deployed_path, problems):
    local = rules_by_key(load(local_path), "alert")
    deployed = rules_by_key(load(deployed_path), "alert")

    for name in sorted(set(local) - set(deployed)):
        problems.append("alert %s is local-only; a deployed environment would never fire it" % name)
    for name in sorted(set(deployed) - set(local)):
        problems.append("alert %s is deployed-only; nothing exercises it locally" % name)

    for name in sorted(set(local) & set(deployed)):
        _, local_rule = local[name]
        _, deployed_rule = deployed[name]

        if normalise(local_rule.get("expr")) != normalise(deployed_rule.get("expr")):
            problems.append(
                "alert %s has different expressions:\n      local:    %s\n      deployed: %s"
                % (name, normalise(local_rule.get("expr")), normalise(deployed_rule.get("expr")))
            )

        local_severity = (local_rule.get("labels") or {}).get("severity")
        deployed_severity = (deployed_rule.get("labels") or {}).get("severity")
        if local_severity != deployed_severity:
            problems.append(
                "alert %s is %s locally and %s deployed; severity decides whether someone is woken up"
                % (name, local_severity, deployed_severity)
            )

        try:
            local_for = seconds(local_rule.get("for"))
            deployed_for = seconds(deployed_rule.get("for"))
        except ValueError as err:
            problems.append("alert %s: %s" % (name, err))
            continue
        if deployed_for < local_for:
            problems.append(
                "alert %s waits %s deployed but %s locally; a deployed environment needs at least "
                "the local patience, or a rolling update pages someone"
                % (name, deployed_rule.get("for"), local_rule.get("for"))
            )


def main(argv):
    if len(argv) != 3:
        sys.stderr.write("usage: check-alert-rules.py <local-rules-dir> <deployed-rules-dir>\n")
        return 2
    local_dir, deployed_dir = argv[1], argv[2]

    problems = []
    check_recording(local_dir + "/recording.yml", deployed_dir + "/recording.yml", problems)
    check_alerts(local_dir + "/alerts.yml", deployed_dir + "/alerts.yml", problems)

    if problems:
        for problem in problems:
            sys.stderr.write("      %s\n" % problem)
        return 1

    local_alerts = rules_by_key(load(local_dir + "/alerts.yml"), "alert")
    local_records = rules_by_key(load(local_dir + "/recording.yml"), "record")
    if not local_alerts or not local_records:
        sys.stderr.write("      read no rules; the check is not exercising anything\n")
        return 1
    print("%d recording rules identical, %d alerts matched" % (len(local_records), len(local_alerts)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
