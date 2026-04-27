'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import type { UIEvent as ReactUIEvent } from 'react';
import { Button } from '@heroui/react';
import { Check, Copy } from 'lucide-react';
import { codeToHtml } from 'shiki';

interface CodeBlockProps {
  value: string;
  copyValue?: string;
  language?: string;
  maxHeight?: string;
}

const FORMAT_CHAR_LIMIT = 200_000;
const HIGHLIGHT_CHAR_LIMIT = 120_000;
const HTML_CACHE_LIMIT = 24;
const TEXT_LAZY_INITIAL_CHARS = 80_000;
const TEXT_LAZY_CHUNK_CHARS = 80_000;
const TEXT_LAZY_THRESHOLD_PX = 640;
const htmlCache = new Map<string, string>();

export function CodeBlock({ value, copyValue, language = 'json', maxHeight = '420px' }: CodeBlockProps) {
  const [html, setHtml] = useState('');
  const [copied, setCopied] = useState(false);

  const normalized = useMemo(() => {
    if (language === 'json' && value.length <= FORMAT_CHAR_LIMIT) {
      try {
        return JSON.stringify(JSON.parse(value), null, 2);
      } catch {
        return value;
      }
    }
    return value;
  }, [language, value]);
  const shouldHighlight = normalized.length <= HIGHLIGHT_CHAR_LIMIT;
  const {
    hasMoreText,
    loadMoreText,
    onScroll: handleTextScroll,
    visibleChars,
    visibleText,
  } = useScrollLazyText(normalized, !shouldHighlight);

  useEffect(() => {
    if (!shouldHighlight) {
      return;
    }

    let cancelled = false;
    const cacheKey = `${language}:${normalized}`;
    const cached = htmlCache.get(cacheKey);
    if (cached) {
      queueMicrotask(() => {
        if (!cancelled) {
          setHtml(cached);
        }
      });
      return;
    }

    codeToHtml(normalized || ' ', {
      lang: language,
      themes: {
        light: 'github-light',
        dark: 'github-dark',
      },
    }).then((result) => {
      if (!cancelled) {
        htmlCache.set(cacheKey, result);
        while (htmlCache.size > HTML_CACHE_LIMIT) {
          const oldest = htmlCache.keys().next().value;
          if (!oldest) break;
          htmlCache.delete(oldest);
        }
        setHtml(result);
      }
    });
    return () => {
      cancelled = true;
    };
  }, [language, normalized, shouldHighlight]);

  const copy = async () => {
    await navigator.clipboard.writeText(copyValue ?? normalized);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1200);
  };

  return (
    <div className="relative min-w-0 overflow-hidden rounded-md border border-divider bg-content1">
      <Button
        isIconOnly
        size="sm"
        variant="ghost"
        className="absolute right-2 top-2 z-10"
        onPress={copy}
        aria-label="Copy code"
      >
        {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
      </Button>
      {shouldHighlight ? (
        <div
          className="code-block overflow-auto text-xs"
          style={{ maxHeight }}
          dangerouslySetInnerHTML={{ __html: html }}
        />
      ) : (
        <>
          <pre className="code-block overflow-auto whitespace-pre-wrap break-words p-4 text-xs" style={{ maxHeight }} onScroll={handleTextScroll}>
            {visibleText}
          </pre>
          {hasMoreText && (
            <div className="border-t border-divider px-3 py-2 text-[11px] text-default-500">
              <div className="mb-1 flex items-center justify-between gap-3">
                <span>Rendered text</span>
                <span className="font-mono">{visibleChars.toLocaleString()}/{normalized.length.toLocaleString()}</span>
              </div>
              <div className="mb-2 flex items-center justify-between gap-3">
                <span>Scroll to load more</span>
                <button type="button" className="text-primary hover:underline" onClick={loadMoreText}>
                  Load more
                </button>
              </div>
              <div className="h-1 overflow-hidden rounded-full bg-default-200">
                <span
                  className="block h-full bg-primary"
                  style={{ width: `${Math.round((visibleChars / Math.max(normalized.length, 1)) * 100)}%` }}
                />
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function useScrollLazyText(value: string, enabled: boolean) {
  const [visibleChars, setVisibleChars] = useState(() => (
    enabled ? Math.min(value.length, TEXT_LAZY_INITIAL_CHARS) : value.length
  ));
  const minimumChars = enabled ? Math.min(value.length, TEXT_LAZY_INITIAL_CHARS) : value.length;

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) {
        setVisibleChars(minimumChars);
      }
    });
    return () => {
      cancelled = true;
    };
  }, [minimumChars, value]);

  const loadMoreText = useCallback(() => {
    setVisibleChars((current) => Math.min(value.length, Math.max(current, minimumChars) + TEXT_LAZY_CHUNK_CHARS));
  }, [minimumChars, value.length]);

  const onScroll = useCallback((event: ReactUIEvent<HTMLElement>) => {
    if (!enabled) return;
    const element = event.currentTarget;
    const remaining = element.scrollHeight - element.scrollTop - element.clientHeight;
    if (remaining <= TEXT_LAZY_THRESHOLD_PX) {
      loadMoreText();
    }
  }, [enabled, loadMoreText]);

  const safeVisibleChars = Math.min(visibleChars, value.length);
  return {
    hasMoreText: enabled && safeVisibleChars < value.length,
    loadMoreText,
    onScroll,
    visibleText: enabled ? value.slice(0, safeVisibleChars) : value,
    visibleChars: safeVisibleChars,
  };
}
