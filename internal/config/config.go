// Package config lê a configuração do ambiente.
//
// TUDO entra por variável de ambiente, e nenhum default aponta para instalação real. A razão não é
// estética: caminho de instalação, nome de perfil e nome de bucket identificam o convênio, e este
// repositório é público. Um default "conveniente" viraria vazamento no primeiro `git push`.
//
// Há DOIS conjuntos de variáveis, e a separação tem consequência prática:
//
//   - `VAN_AGENT_*` descreve a MÁQUINA — pastas do cliente STCP, executável, perfil, registro de
//     intenção, padrão de nome. Nada disso existe fora da instalação Windows.
//   - `VAN_S3_*` descreve o BUCKET, e é o MESMO conjunto que o core-api lê em `van-s3-config.ts`,
//     com as mesmas regras. Os dois lados leem os mesmos nomes de propósito: um agente que
//     procurasse remessa num prefixo e um emissor que a depositasse noutro produziriam uma fila
//     silenciosamente vazia, sem erro em lugar nenhum.
//
// Credencial NÃO é o caminho de produção. A autenticação com o object storage é por role da
// instância (ADR-0061 §5), resolvida pela cadeia de provedores do SDK. As chaves estáticas existem
// para o endpoint local do teste de integração, e são XOR: uma sozinha é erro, nunca configuração
// pela metade.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/spool"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
)

const (
	agentPrefix = "VAN_AGENT_"
	// s3Prefix é o namespace COMPARTILHADO com o core-api. Renomear aqui sem renomear lá quebra a
	// fronteira nos dois sentidos.
	s3Prefix = "VAN_S3_"
)

// Config é tudo que o agente precisa saber para rodar um ciclo na máquina.
//
// O NOME DO BUCKET não está aqui de propósito: ele só é necessário no modo que fala com o
// armazenamento real, e mora em `StorageConfig`. O modo de ensaio precisa ser rodável numa
// instalação que ainda não tem bucket algum atribuído.
type Config struct {
	Prefixes bucket.Prefixes
	Spool    spool.Config
	Client   stcp.CommandConfig
	// LedgerDir é onde a intenção é gravada. Precisa ser um caminho LOCAL e persistente: um
	// diretório temporário ou um compartilhamento de rede quebrariam a garantia de que o registro
	// sobrevive à morte do processo.
	LedgerDir string
	// NamePattern decide o que é remessa nossa (CA7).
	NamePattern *regexp.Regexp
	// NameMaxLength é o teto de comprimento do nome de remessa. Zero (o default) significa SEM
	// trava, e a ausência é deliberada: o teto efetivo depende da instalação e do parceiro, e um
	// default que recusasse por engano pararia a fila inteira sem que ninguém tivesse pedido.
	NameMaxLength int
}

// Missing lista variáveis obrigatórias ausentes, para que o boot diga TODAS de uma vez em vez de
// uma por execução.
type Missing []string

func (m Missing) Error() string {
	return fmt.Sprintf("configuração incompleta; faltam as variáveis: %v", []string(m))
}

// InvalidPrefixError é o prefixo que não dá para normalizar.
//
// Espelha o `invalid-prefix` do core-api, e nomeia a variável porque quem lê o erro está numa
// máquina, sem o código à mão.
type InvalidPrefixError struct {
	Field string
	Raw   string
}

func (e InvalidPrefixError) Error() string {
	return fmt.Sprintf("%s inválido (%q): prefixo não pode começar com barra", e.Field, e.Raw)
}

func lookup(prefix, name string, missing *Missing) string {
	v := os.Getenv(prefix + name)
	if v == "" {
		*missing = append(*missing, prefix+name)
	}
	return v
}

func lookupWithDefault(prefix, name, fallback string) string {
	if v := os.Getenv(prefix + name); v != "" {
		return v
	}
	return fallback
}

func lookupInt(prefix, name string, fallback int) (int, error) {
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

// normalizePrefix acrescenta a barra final quando ela falta, e recusa prefixo iniciado por barra.
//
// A barra final NÃO é conveniência: sem ela, `saida` + `ARQUIVO.REM` vira `saidaARQUIVO.REM` — um
// objeto na raiz do bucket, que nenhuma listagem por prefixo encontra. A remessa sumiria sem erro
// em lugar nenhum. Acrescentar em vez de recusar espelha o que o core-api faz (`van-s3-config.ts`),
// e os dois lados precisam concordar sobre o mesmo valor de ambiente.
//
// Barra INICIAL é outra história e é recusada: no S3 ela produz um segmento vazio no começo da
// chave, e `/saida/X.REM` é um objeto diferente de `saida/X.REM`.
func normalizePrefix(raw, fallback, field string) (string, error) {
	if raw == "" {
		return fallback, nil
	}
	if strings.HasPrefix(raw, "/") {
		return "", InvalidPrefixError{Field: field, Raw: raw}
	}
	if !strings.HasSuffix(raw, "/") {
		return raw + "/", nil
	}
	return raw, nil
}

// Load lê o ambiente e recusa configuração incompleta.
//
// Falha no BOOT, nunca no meio de um ciclo. Um agente que descobre configuração faltando depois de
// depositar um arquivo na pasta de saída já enfileirou um pagamento que não sabe acompanhar.
func Load() (Config, error) {
	var missing Missing

	prefixes, err := loadPrefixes()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		LedgerDir: lookup(agentPrefix, "LEDGER_DIR", &missing),
		Prefixes:  prefixes,
		Spool: spool.Config{
			OutboundDir: lookup(agentPrefix, "STCP_OUTBOUND_DIR", &missing),
			BackupDir:   lookup(agentPrefix, "STCP_BACKUP_DIR", &missing),
			LogDir:      lookup(agentPrefix, "STCP_LOG_DIR", &missing),
			// As pastas da recepção são OPCIONAIS na leitura e cobradas pelo ciclo que as usa: uma
			// instalação que ainda só transmite precisa continuar bootando, e um boot que falhasse
			// por causa de uma pasta que o modo em execução não toca transformaria trabalho pendente
			// em parada de produção.
			InboundDir:  os.Getenv(agentPrefix + "STCP_INBOUND_DIR"),
			ReceivedDir: os.Getenv(agentPrefix + "STCP_RECEIVED_DIR"),
			// O nome do arquivo do log POSICIONAL não é documentado pelo manual v5.3 — ele descreve
			// o layout (§12, p.30) e o diretório, mas não o nome. Fica configurável, com um padrão
			// que precisa ser confirmado contra a instalação real.
			TransferLogGlob: lookupWithDefault(agentPrefix, "STCP_TRANSFER_LOG_GLOB", "*.LOG"),
		},
		Client: stcp.CommandConfig{
			ExecutablePath: lookup(agentPrefix, "STCP_EXE", &missing),
			ConfigPath:     lookup(agentPrefix, "STCP_INI", &missing),
			Profile:        lookup(agentPrefix, "STCP_PROFILE", &missing),
		},
	}

	retries, err := lookupInt(agentPrefix, "STCP_RETRIES", 5)
	if err != nil {
		return Config{}, err
	}
	interval, err := lookupInt(agentPrefix, "STCP_RETRY_INTERVAL_SECONDS", 30)
	if err != nil {
		return Config{}, err
	}
	cfg.Client.Retries = retries
	cfg.Client.RetryIntervalSeconds = interval

	maxLen, err := lookupInt(agentPrefix, "NAME_MAX_LENGTH", 0)
	if err != nil {
		return Config{}, err
	}
	if maxLen < 0 {
		return Config{}, fmt.Errorf("%sNAME_MAX_LENGTH não pode ser negativo; veio %d", agentPrefix, maxLen)
	}
	cfg.NameMaxLength = maxLen

	rawPattern := lookup(agentPrefix, "NAME_PATTERN", &missing)
	if rawPattern != "" {
		pattern, err := regexp.Compile(rawPattern)
		if err != nil {
			return Config{}, fmt.Errorf("%sNAME_PATTERN inválido (%q): %w", agentPrefix, rawPattern, err)
		}
		// Um padrão sem âncoras casaria por substring, e "casa em algum lugar do nome" é uma trava
		// que não trava: `PAG_` casaria com `NAO_E_PAG_NOSSO.txt`.
		if pattern.String() == "" || pattern.String()[0] != '^' || pattern.String()[len(pattern.String())-1] != '$' {
			return Config{}, fmt.Errorf(
				"%sNAME_PATTERN precisa ancorar o nome inteiro (começar com ^ e terminar com $); veio %q",
				agentPrefix, rawPattern)
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

// loadPrefixes lê os cinco prefixos do contrato.
//
// São opcionais com default: quem renomeia prefixo é a infra, não este código — mas o default
// precisa bater com o do core-api, e é por isso que ele vem de `bucket.DefaultPrefixes()` em vez de
// literais soltos aqui.
func loadPrefixes() (bucket.Prefixes, error) {
	defaults := bucket.DefaultPrefixes()

	type slot struct {
		field    string
		fallback string
		into     *string
	}
	var out bucket.Prefixes
	for _, s := range []slot{
		{s3Prefix + "PREFIX_OUTBOUND", defaults.Outbound, &out.Outbound},
		{s3Prefix + "PREFIX_PROCESSED", defaults.Processed, &out.Processed},
		{s3Prefix + "PREFIX_FAILED", defaults.Failed, &out.Failed},
		{s3Prefix + "PREFIX_RETURNS", defaults.Returns, &out.Returns},
		{s3Prefix + "PREFIX_STATUS", defaults.Status, &out.Status},
	} {
		normalized, err := normalizePrefix(os.Getenv(s.field), s.fallback, s.field)
		if err != nil {
			return bucket.Prefixes{}, err
		}
		*s.into = normalized
	}
	return out, nil
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
	if c.NameMaxLength < 0 {
		return fmt.Errorf("teto de comprimento do nome negativo: %d", c.NameMaxLength)
	}
	return nil
}

// LoadStorage lê o que é preciso para falar com o bucket real.
//
// É SEPARADA de `Load` porque o modo de ensaio não deve exigi-la: ensaiar uma instalação nova é
// justamente o que se faz antes de ela ter bucket, credencial ou rede — e um ensaio que falhasse
// por falta de `VAN_S3_BUCKET` não verificaria nada do que ele existe para verificar.
func LoadStorage() (bucket.S3Config, error) {
	var missing Missing

	cfg := bucket.S3Config{
		Bucket:   lookup(s3Prefix, "BUCKET", &missing),
		Region:   lookup(s3Prefix, "REGION", &missing),
		Endpoint: os.Getenv(s3Prefix + "ENDPOINT"),
	}

	pathStyle, err := forcePathStyle(cfg.Endpoint)
	if err != nil {
		return bucket.S3Config{}, err
	}
	cfg.ForcePathStyle = pathStyle

	keyID := os.Getenv(s3Prefix + "ACCESS_KEY_ID")
	secret := os.Getenv(s3Prefix + "SECRET_ACCESS_KEY")
	switch {
	case keyID != "" && secret == "":
		// XOR: nomeia a que falta. Metade da credencial nunca é intenção — é uma variável que ficou
		// para trás, e tentar assinar com ela produz um erro de autenticação longe da causa.
		missing = append(missing, s3Prefix+"SECRET_ACCESS_KEY")
	case keyID == "" && secret != "":
		missing = append(missing, s3Prefix+"ACCESS_KEY_ID")
	case keyID != "" && secret != "":
		cfg.Credentials = &bucket.StaticCredentials{AccessKeyID: keyID, SecretAccessKey: secret}
	}
	// Nenhuma das duas: cadeia de provedores (role da instância). É o caminho de produção, e é o
	// único em que nada de credencial existe em disco ou em variável (ADR-0061 §5).

	if len(missing) > 0 {
		return bucket.S3Config{}, missing
	}
	if err := cfg.Validate(); err != nil {
		return bucket.S3Config{}, err
	}
	return cfg, nil
}

// localHost casa o que o core-api considera endpoint local (`van-s3-config.ts`). Os dois lados
// precisam decidir path-style da mesma forma nos casos que os dois cobrem, ou um deles fala com um
// host que não existe.
var localHost = regexp.MustCompile(`localhost|127\.0\.0\.1|0\.0\.0\.0`)

// forcePathStyle decide o endereçamento das chaves: por caminho (`endpoint/bucket/chave`) ou por
// subdomínio (`bucket.endpoint/chave`).
//
// O default é a heurística do core-api — endpoint local ⇒ por caminho —, porque `bucket.localhost`
// não resolve e sem isso o erro que aparece é de DNS, sem dizer nada sobre a causa.
//
// ⚠️ `VAN_S3_FORCE_PATH_STYLE` é ADIÇÃO nossa: não existe do lado do core-api. Ela foi necessária
// ao exercitar o adapter contra um S3-compatível que não está em localhost — um endereço de rede
// interna passa pela heurística como se fosse a AWS, e o SDK vai procurar um subdomínio que não
// existe. Não definida, o comportamento é idêntico ao do core-api; é por isso que ela pôde entrar
// sem que a outra metade da fronteira mudasse junto.
func forcePathStyle(endpoint string) (bool, error) {
	raw := os.Getenv(s3Prefix + "FORCE_PATH_STYLE")
	if raw == "" {
		return endpoint != "" && localHost.MatchString(endpoint), nil
	}
	valor, err := strconv.ParseBool(raw)
	if err != nil {
		// Um valor que não dá para interpretar NÃO cai no default em silêncio: quem escreveu a
		// variável tinha uma intenção, e adivinhá-la produziria um agente que fala com o host errado
		// achando que obedeceu.
		return false, fmt.Errorf("%sFORCE_PATH_STYLE inválido (%q): %w", s3Prefix, raw, err)
	}
	return valor, nil
}
