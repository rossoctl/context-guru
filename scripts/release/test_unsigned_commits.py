"""Self-check of unsigned-commits.sh, the DCO rule of check-ready.sh: python test_unsigned_commits.py  (needs jq)."""
import json, os, subprocess, sys
HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "unsigned-commits.sh")
SHA_A = "a" * 40
SHA_B = "b" * 40
SHA_C = "c" * 40
ME = ("DAVID BOAZ", "DAVIDBO@il.ibm.com")
SIGN = "Signed-off-by: DAVID BOAZ <DAVIDBO@il.ibm.com>"

def commit(sha, message, author=ME):
    return {"sha": sha, "commit": {"message": message, "author": {"name": author[0], "email": author[1]}}}

def remediation(sha, who=ME, signed=True):
    body = f"chore: DCO remediation\n\nI, {who[0]} <{who[1]}>, hereby add my Signed-off-by to this commit: {sha}\n"
    return body + (f"\n{SIGN}\n" if signed else "")

def unsigned(commits):
    r = subprocess.run([SCRIPT], input=json.dumps(commits), capture_output=True, text=True, check=True)
    return r.stdout.split()

def test_every_commit_signed():
    assert unsigned([commit(SHA_A, f"feat: a\n\n{SIGN}\n"), commit(SHA_B, f"feat: b\n\n{SIGN}\n")]) == []

def test_unsigned_commit_is_listed_by_short_sha():
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, f"feat: b\n\n{SIGN}\n")]) == [SHA_A[:12]]

def test_a_remediation_commit_signs_it_off():
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, remediation(SHA_A))]) == []

def test_a_remediation_line_matches_without_regard_to_case():
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, remediation(SHA_A.upper()))]) == []
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, remediation(SHA_A, ("david boaz", "davidbo@IL.ibm.com")))]) == []

def test_a_short_sha_does_not_count():
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, remediation(SHA_A[:12]))]) == [SHA_A[:12]]

def test_another_person_cannot_sign_off_for_the_author():
    other = ("Someone Else", "else@example.com")
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, remediation(SHA_A, other), author=other)]) == [SHA_A[:12]]

def test_the_name_and_email_must_both_match_the_unsigned_commit():
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, remediation(SHA_A, ("DAVID BOAZ", "other@example.com")))]) == [SHA_A[:12]]
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, remediation(SHA_A, ("Another Name", ME[1])))]) == [SHA_A[:12]]

def test_a_remediation_commit_without_its_own_sign_off_does_not_count():
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, remediation(SHA_A, signed=False))]) == [SHA_A[:12], SHA_B[:12]]

def test_a_remediation_for_one_commit_does_not_cover_another():
    assert unsigned([commit(SHA_A, "feat: a"), commit(SHA_B, "feat: b"), commit(SHA_C, remediation(SHA_A))]) == [SHA_B[:12]]

def test_a_commit_cannot_remediate_itself():
    assert unsigned([commit(SHA_A, remediation(SHA_A, signed=False))]) == [SHA_A[:12]]

if __name__ == "__main__":
    failed = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn(); print("ok  ", name)
            except AssertionError:
                failed += 1; print("FAIL", name)
    sys.exit(1 if failed else 0)
