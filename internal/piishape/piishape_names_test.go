package piishape

import "testing"

// TestCommonCredentialNamesAreRecognised: names that carry a credential in shell commands, env files
// and config, and that the first key-word list missed: no boundary before the word (PGPASSWORD), a
// shorter word (a password or pwd that is not spelled out), compound key names, a trailing id or
// value, a plural and a digit.
func TestCommonCredentialNamesAreRecognised(t *testing.T) {
	v := valueFixture(91, 15, alnum)
	for _, name := range []string{
		"PG" + "PASSWORD=",
		"MYSQL_" + "PWD=",
		"DB_" + "PASS=",
		"pass" + "phrase=",
		"PRIVATE_" + "KEY=",
		"ACCESS_" + "KEY=",
		"AUTH_" + "KEY=",
		"cre" + "dentials=",
		"client_" + "secret_value=",
		"secret_" + "id=",
		"token_" + "value=",
		"pass" + "words: ",
		"pass" + "word1=",
	} {
		text := "export " + name + v + " psql"
		if len(Matching(text)) == 0 {
			t.Errorf("%q is not recognised", text)
		}
	}
}

// TestOrdinaryWordsNearTheNewNamesStayQuiet: the wider list must not read test output, the shell's
// working directory or a word that merely ends in one of the new names as a credential.
func TestOrdinaryWordsNearTheNewNamesStayQuiet(t *testing.T) {
	for _, text := range []string{
		"--- PASS: TestSomethingWithALongName (0.00s)",
		"PASS: TestSomethingWithALongName/sub_case_name",
		"PWD=/srv/project/sub/dir",
		"OLDPWD=C:/some/long/directory/name/here",
		"bypass: some-long-value-here-ok",
		"compass=north-north-east-ish-heading",
		"credentials_file=/etc/creds/the/file",
		"private_key_path=/etc/ssl/private/x.pem",
		"token_count: 1234567890123",
	} {
		if got := Matching(text); got != nil {
			t.Errorf("Matching(%q) = %v, want nil", text, got)
		}
	}
}
