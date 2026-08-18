// Package config lê a configuração do ambiente.
//
// TUDO entra por variável de ambiente, e nenhum default aponta para instalação real. A razão não é
// estética: caminho de instalação, nome de perfil e nome de bucket identificam o convênio, e este
// repositório é público. Um default "conveniente" viraria vazamento no primeiro `git push`.
//
// Credencial NÃO se lê aqui. A autenticação com o object storage é por role da instância
// (ADR-0061 §5), resolvida pela cadeia de provedores do SDK — nenhuma chave em disco, nenhuma
// chave em variável.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/spool"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
)

const prefix = "VAN_AGENT_"

// Config é tudo que o agente precisa saber para rodar.
type Config struct {
	// BucketName identifica o bucket. Homologação e produção são buckets SEPARADOS: escrever no
	// lugar errado precisa exigir trocar isto, não um prefixo (ADR-0061 §1).
	BucketName string
	Prefixes   bucket.Prefixes
	Spool      spool.Config
	Client     stcp.CommandConfig
	// LedgerDir é onde a intenção é gravada. Precisa ser um caminho LOCAL e persistente: um
	// diretório temporário ou um compartilhamento de rede quebrariam a garantia de que o registro
	// sobrevive à morte do processo.
	LedgerDir string
	// NamePattern decide o que é remessa nossa (CA7).
	NamePattern *regexp.Regexp
}

// Missing lista variáveis obrigatórias ausentes, para que o boot diga TODAS de uma vez em vez de
// uma por execução.
type Missing []string

func (m Missing) Error() string {
	return fmt.Sprintf("configuração incompleta; faltam as variáveis: %v", []string(m))
}

func lookup(name string, missing *Missing) string {
	v := os.Getenv(prefix + name)
	if v == "" {
		*missing = append(*missing, prefix+name)
	}
	return v
}

func lookupWithDefault(name, fallback string) string {
	if v := os.Getenv(prefix + name); v != "" {
		return v
	}
	return fallback
}

func lookupInt(name string, fallback int) (int, error) {
	raw := os.Getenv(prefix + name)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s%s inválido (%q): %w", prefix, name, raw, err)
	}
	return v, nil
}

// Load lê o ambiente e recusa configuração incompleta.
//
// Falha no BOOT, nunca no meio de um ciclo. Um agente que descobre configuração faltando depois de
// depositar um arquivo na pasta de saída já enfileirou um pagamento que não sabe acompanhar.
func Load() (Config, error) {
	var missing Missing

	cfg := Config{
		BucketName: lookup("BUCKET", &missing),
		LedgerDir:  lookup("LEDGER_DIR", &missing),
		Prefixes: bucket.Prefixes{
			Outbound:  lookupWithDefault("PREFIX_OUTBOUND", bucket.DefaultPrefixes().Outbound),
			Processed: lookupWithDefault("PREFIX_PROCESSED", bucket.DefaultPrefixes().Processed),
			Failed:    lookupWithDefault("PREFIX_FAILED", bucket.DefaultPrefixes().Failed),
			Returns:   lookupWithDefault("PREFIX_RETURNS", bucket.DefaultPrefixes().Returns),
			Status:    lookupWithDefault("PREFIX_STATUS", bucket.DefaultPrefixes().Status),
		},
		Spool: spool.Config{
			OutboundDir: lookup("STCP_OUTBOUND_DIR", &missing),
			BackupDir:   lookup("STCP_BACKUP_DIR", &missing),
			LogDir:      lookup("STCP_LOG_DIR", &missing),
			// O nome do arquivo do log POSICIONAL não é documentado pelo manual v5.3 — ele descreve
			// o layout (§12, p.30) e o diretório, mas não o nome. Fica configurável, com um padrão
			// que precisa ser confirmado contra a instalação real.
			TransferLogGlob: lookupWithDefault("STCP_TRANSFER_LOG_GLOB", "*.LOG"),
		},
		Client: stcp.CommandConfig{
			ExecutablePath: lookup("STCP_EXE", &missing),
			ConfigPath:     lookup("STCP_INI", &missing),
			Profile:        lookup("STCP_PROFILE", &missing),
		},
	}

	retries, err := lookupInt("STCP_RETRIES", 5)
	if err != nil {
		return Config{}, err
	}
	interval, err := lookupInt("STCP_RETRY_INTERVAL_SECONDS", 30)
	if err != nil {
		return Config{}, err
	}
	cfg.Client.Retries = retries
	cfg.Client.RetryIntervalSeconds = interval

	rawPattern := lookup("NAME_PATTERN", &missing)
	if rawPattern != "" {
		pattern, err := regexp.Compile(rawPattern)
		if err != nil {
			return Config{}, fmt.Errorf("%sNAME_PATTERN inválido (%q): %w", prefix, rawPattern, err)
		}
		// Um padrão sem âncoras casaria por substring, e "casa em algum lugar do nome" é uma trava
		// que não trava: `PAG_` casaria com `NAO_E_PAG_NOSSO.txt`.
		if pattern.String() == "" || pattern.String()[0] != '^' || pattern.String()[len(pattern.String())-1] != '$' {
			return Config{}, fmt.Errorf(
				"%sNAME_PATTERN precisa ancorar o nome inteiro (começar com ^ e terminar com $); veio %q",
				prefix, rawPattern)
		}
		cfg.NamePattern = pattern
	}

	if len(missing) > 0 {
		return Config{}, missing
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate confere o que a leitura sozinha não garante.
func (c Config) Validate() error {
	if err := c.Prefixes.Validate(); err != nil {
		return err
	}
	if err := c.Spool.Validate(); err != nil {
		return err
	}
	if err := c.Client.Validate(); err != nil {
		return err
	}
	if c.NamePattern == nil {
		return errors.New("padrão de nome de remessa não configurado")
	}
	return nil
}
