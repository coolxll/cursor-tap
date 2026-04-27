declare module 'shiki' {
  export function codeToHtml(
    code: string,
    options: {
      lang: string;
      theme?: string;
      themes?: {
        light: string;
        dark: string;
      };
    },
  ): Promise<string>;
}
